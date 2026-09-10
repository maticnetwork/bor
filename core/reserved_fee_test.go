package core

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

var reservedBurntAddr = common.HexToAddress("0x00000000000000000000000000000000000000dd")

// reservedTestConfig clones BorUnittestChainConfig (London active at 0) and
// layers the reserved-blockspace fork on top, without mutating the shared
// global config. reservedSenders is accepted for call-site symmetry with
// reservedBlockCtx (which builds the actual classification snapshot from
// them); the config itself carries no reserved-client data — classification
// is sourced from the registry snapshot on the block context, not config.
func reservedTestConfig(forkBlock *big.Int, reservedSenders ...common.Address) *params.ChainConfig {
	cc := *params.BorUnittestChainConfig
	bor := *cc.Bor
	bor.BurntContract = map[string]string{"0": reservedBurntAddr.Hex()}
	bor.ReservedBlockspaceBlock = forkBlock
	cc.Bor = &bor
	return &cc
}

// reservedBlockCtx builds a block context with a reserved-set snapshot covering
// reservedSenders (the registry-backed source the EVM now classifies from).
func reservedBlockCtx(coinbase common.Address, blockNumber *big.Int, baseFee *big.Int, reservedSenders ...common.Address) vm.BlockContext {
	ctx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		GasLimit:    30_000_000,
		BlockNumber: blockNumber,
		Time:        1,
		BaseFee:     baseFee,
	}
	if len(reservedSenders) > 0 {
		clients := make(map[common.Address]registryreader.Client, len(reservedSenders))
		reserved := make(map[registryreader.ReservedKey]struct{}, len(reservedSenders))
		for i, a := range reservedSenders {
			clients[a] = registryreader.Client{ID: uint64(i + 1), GasQuota: 30_000_000}
			// These direct-ApplyMessage tests all use nonce-0 reserved txs; the
			// processor builds this set from the ordered body via ClassifyReserved,
			// but here we populate it directly since there is no processor.
			reserved[registryreader.ReservedKey{From: a, Nonce: 0}] = struct{}{}
		}
		ctx.ReservedSnapshot = registryreader.NewSnapshot(common.HexToHash("0x1"), uint64(len(reservedSenders))*30_000_000, clients)
		ctx.ReservedTxs = reserved
	}
	return ctx
}

// fundedState returns an in-memory StateDB with `sender` funded and nonce 0.
func fundedState(t *testing.T, sender common.Address, balance *uint256.Int) *state.StateDB {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	sdb.AddBalance(sender, balance, 0)
	sdb.SetNonce(sender, 0, 0)
	return sdb
}

// TestReservedTxSkipsFees pins the core behaviour: a reserved-sender
// tx executes fee-free past the fork — no gas debit, no base-fee floor, no
// producer tip, no burn. Only msg.Value moves.
func TestReservedTxSkipsFees(t *testing.T) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000c0b0")
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")

	cc := reservedTestConfig(big.NewInt(0), sender)
	baseFee := big.NewInt(1_000_000_000)
	blockCtx := reservedBlockCtx(coinbase, big.NewInt(1), baseFee, sender)

	initial := uint256.NewInt(1e18)
	sdb := fundedState(t, sender, initial)

	value := big.NewInt(5)
	signer := types.NewLondonSigner(cc.ChainID)
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cc.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(0), // below baseFee — would be ErrFeeCapTooLow if not reserved
		Gas:       21000,
		To:        &recipient,
		Value:     value,
	}), signer, key)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := TransactionToMessage(tx, signer, baseFee)
	if err != nil {
		t.Fatal(err)
	}

	evm := vm.NewEVM(blockCtx, sdb, cc, vm.Config{})
	evm.SetTxContext(NewEVMTxContext(msg))
	result, err := ApplyMessage(evm, msg, new(GasPool).AddGas(blockCtx.GasLimit))
	if err != nil {
		t.Fatalf("reserved tx failed: %v", err)
	}
	if result.Failed() {
		t.Fatalf("reserved tx reverted: %v", result.Err)
	}

	// No fee was applied in-protocol.
	if result.FeeBurnt.Sign() != 0 {
		t.Errorf("FeeBurnt=%s, want 0", result.FeeBurnt)
	}
	if result.FeeTipped.Sign() != 0 {
		t.Errorf("FeeTipped=%s, want 0", result.FeeTipped)
	}
	if got := sdb.GetBalance(coinbase); !got.IsZero() {
		t.Errorf("coinbase tip=%s, want 0", got)
	}
	if got := sdb.GetBalance(reservedBurntAddr); !got.IsZero() {
		t.Errorf("burnt-contract balance=%s, want 0", got)
	}

	// Sender paid only value — no gas was debited.
	wantSender := new(uint256.Int).Sub(initial, uint256.MustFromBig(value))
	if got := sdb.GetBalance(sender); got.Cmp(wantSender) != 0 {
		t.Errorf("sender balance=%s, want %s (value only, no gas)", got, wantSender)
	}
	if got := sdb.GetBalance(recipient); got.Cmp(uint256.MustFromBig(value)) != 0 {
		t.Errorf("recipient balance=%s, want %s", got, value)
	}
	if result.UsedGas != 21000 {
		t.Errorf("UsedGas=%d, want 21000", result.UsedGas)
	}
}

// TestReservedTxGatedByFork verifies the fork gate: before the activation
// block a reserved sender gets no exemption, so a zero-fee tx is rejected
// with ErrFeeCapTooLow exactly as any other underpriced tx.
func TestReservedTxGatedByFork(t *testing.T) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")

	cc := reservedTestConfig(big.NewInt(100), sender) // fork at 100
	baseFee := big.NewInt(1_000_000_000)
	blockCtx := reservedBlockCtx(common.HexToAddress("0xc0b0"), big.NewInt(1), baseFee, sender) // pre-fork

	sdb := fundedState(t, sender, uint256.NewInt(1e18))
	signer := types.NewLondonSigner(cc.ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cc.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(0),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, baseFee)

	evm := vm.NewEVM(blockCtx, sdb, cc, vm.Config{})
	evm.SetTxContext(NewEVMTxContext(msg))
	_, err := ApplyMessage(evm, msg, new(GasPool).AddGas(blockCtx.GasLimit))
	if err == nil {
		t.Fatal("pre-fork zero-fee tx should be rejected, got nil error")
	}
}

// TestNonReservedSenderPaysNormally verifies the negative case: with the fork
// active but the sender NOT a reserved client, fees apply as usual (tip to
// coinbase, base fee burnt).
func TestNonReservedSenderPaysNormally(t *testing.T) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000c0b0")
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")

	// Fork active, but the reserved client is a DIFFERENT address.
	reserved := common.HexToAddress("0x00000000000000000000000000000000000000ee")
	cc := reservedTestConfig(big.NewInt(0), reserved)
	baseFee := big.NewInt(1_000_000_000)
	blockCtx := reservedBlockCtx(coinbase, big.NewInt(1), baseFee, reserved)

	sdb := fundedState(t, sender, uint256.NewInt(1e18))
	signer := types.NewLondonSigner(cc.ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cc.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, baseFee)

	evm := vm.NewEVM(blockCtx, sdb, cc, vm.Config{})
	evm.SetTxContext(NewEVMTxContext(msg))
	result, err := ApplyMessage(evm, msg, new(GasPool).AddGas(blockCtx.GasLimit))
	if err != nil {
		t.Fatalf("non-reserved tx failed: %v", err)
	}
	if result.FeeBurnt == nil || result.FeeBurnt.Sign() == 0 {
		t.Errorf("expected non-zero burn for non-reserved sender, got %v", result.FeeBurnt)
	}
	if got := sdb.GetBalance(coinbase); got.IsZero() {
		t.Error("expected non-zero coinbase tip for non-reserved sender")
	}
	if got := sdb.GetBalance(reservedBurntAddr); got.IsZero() {
		t.Error("expected non-zero burnt-contract balance for non-reserved sender")
	}
}

// TestReservedTxSerialParallelParity is the consensus-critical check: the
// reserved fee path must produce a byte-identical post-state whether the tx
// runs through the serial executor (ApplyMessage) or BlockSTM
// (ExecuteV2BlockSTM). The two paths must produce identical results.
func TestReservedTxSerialParallelParity(t *testing.T) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000c0b0")
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")

	cc := reservedTestConfig(big.NewInt(0), sender)
	baseFee := big.NewInt(1_000_000_000)
	blockCtx := reservedBlockCtx(coinbase, big.NewInt(1), baseFee, sender)

	// Committed base state shared by both paths.
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(types.EmptyRootHash, state.NewDatabase(tdb, nil))
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)

	signer := types.NewLondonSigner(cc.ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cc.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(0),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(7),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, baseFee)

	// Serial path.
	serialDB, _ := state.New(root, state.NewDatabase(tdb, nil))
	evm := vm.NewEVM(blockCtx, serialDB, cc, vm.Config{})
	evm.SetTxContext(NewEVMTxContext(msg))
	if _, err := ApplyMessage(evm, msg, new(GasPool).AddGas(blockCtx.GasLimit)); err != nil {
		t.Fatalf("serial reserved tx failed: %v", err)
	}
	serialRoot := serialDB.IntermediateRoot(true)

	// Parallel (BlockSTM) path.
	base, _ := state.New(root, state.NewDatabase(tdb, nil))
	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()
	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}
	result := ExecuteV2BlockSTM(context.Background(), tasks, base,
		blockstm.NewMVStore(), blockstm.NewMVBalanceStore(),
		blockCtx, common.Hash{}, vm.Config{}, cc, blockCtx.GasLimit, 1, finalDB, nil)
	if result.ExecErrIdx >= 0 {
		t.Fatalf("parallel reserved tx errored at %d: %v", result.ExecErrIdx, result.ExecErr)
	}
	if result.PanickedIdx >= 0 {
		t.Fatalf("parallel reserved tx panicked at %d", result.PanickedIdx)
	}
	parallelRoot := finalDB.IntermediateRoot(true)

	if serialRoot != parallelRoot {
		t.Fatalf("state root divergence: serial=%s parallel=%s", serialRoot.Hex(), parallelRoot.Hex())
	}

	// And the balances landed where expected on both.
	for _, db := range []*state.StateDB{serialDB, finalDB} {
		if got := db.GetBalance(coinbase); !got.IsZero() {
			t.Errorf("coinbase tip=%s, want 0", got)
		}
		if got := db.GetBalance(reservedBurntAddr); !got.IsZero() {
			t.Errorf("burnt balance=%s, want 0", got)
		}
		if got := db.GetBalance(recipient); got.Uint64() != 7 {
			t.Errorf("recipient=%s, want 7", got)
		}
	}
}

// TestReservedFallbackFeeWithinQuotaExecutesFeeFree pins POS-3671's execution
// half end to end. Only nonce 0 is classified reserved below (modelling a
// quota that's exhausted by it, as ClassifyReserved would decide for a real
// block); nonce 1 is deliberately absent from ReservedTxs to model the
// overflow case. The sender's balance covers the tx value many times over
// but is nowhere near gas*feeCap, so:
//   - the within-quota tx (nonce 0) executes fee-free (buyGas waives the gas
//     debit entirely, same as TestReservedTxSkipsFees);
//   - the overflow tx (nonce 1), priced on the normal fee path since it isn't
//     in ReservedTxs, fails buyGas with ErrInsufficientFunds before touching
//     any balance — the same clean, pre-execution error any other underfunded
//     sender gets, which is exactly what lets a block builder exclude it
//     without invalidating the block being assembled (mirrors the pool's
//     admission-time waiver in core/txpool/legacypool being quota-unaware:
//     execution is the arbiter for overflow).
func TestReservedFallbackFeeWithinQuotaExecutesFeeFree(t *testing.T) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	coinbase := common.HexToAddress("0x000000000000000000000000000000000000c0b0")
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")

	cc := reservedTestConfig(big.NewInt(0), sender)
	baseFee := big.NewInt(1_000_000_000)

	clients := map[common.Address]registryreader.Client{sender: {ID: 1, GasQuota: 100_000}}
	snap := registryreader.NewSnapshot(common.HexToHash("0x1"), 100_000, clients)
	blockCtx := vm.BlockContext{
		CanTransfer:      CanTransfer,
		Transfer:         Transfer,
		GetHash:          func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:         coinbase,
		GasLimit:         30_000_000,
		BlockNumber:      big.NewInt(1),
		Time:             1,
		BaseFee:          baseFee,
		ReservedSnapshot: snap,
		ReservedTxs:      map[registryreader.ReservedKey]struct{}{{From: sender, Nonce: 0}: {}},
	}

	// gas*feeCap = 21000 * 30 gwei = 6.3e14, far above the funded balance;
	// only the value (1000 wei) is affordable.
	feeCap := big.NewInt(30_000_000_000)
	value := big.NewInt(1_000)
	balance := uint256.NewInt(1_000_000)
	sdb := fundedState(t, sender, balance)
	signer := types.NewLondonSigner(cc.ChainID)

	buildTx := func(nonce uint64) *Message {
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   cc.ChainID,
			Nonce:     nonce,
			GasTipCap: feeCap,
			GasFeeCap: feeCap,
			Gas:       21000,
			To:        &recipient,
			Value:     value,
		}), signer, key)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}

	// nonce 0: within quota, executes fee-free despite the gas-starved balance.
	msg0 := buildTx(0)
	evm := vm.NewEVM(blockCtx, sdb, cc, vm.Config{})
	evm.SetTxContext(NewEVMTxContext(msg0))
	result, err := ApplyMessage(evm, msg0, new(GasPool).AddGas(blockCtx.GasLimit))
	if err != nil {
		t.Fatalf("within-quota fallback-fee tx failed: %v", err)
	}
	if result.Failed() {
		t.Fatalf("within-quota fallback-fee tx reverted: %v", result.Err)
	}
	wantBalance := new(uint256.Int).Sub(balance, uint256.MustFromBig(value))
	if got := sdb.GetBalance(sender); got.Cmp(wantBalance) != 0 {
		t.Fatalf("sender balance=%s, want %s (value only, no gas)", got, wantBalance)
	}

	// nonce 1: overflow (absent from ReservedTxs), priced on the normal fee
	// path where the balance can't cover gas*feeCap. buyGas must reject it
	// cleanly rather than partially applying it.
	balanceBeforeOverflow := sdb.GetBalance(sender)
	msg1 := buildTx(1)
	evm2 := vm.NewEVM(blockCtx, sdb, cc, vm.Config{})
	evm2.SetTxContext(NewEVMTxContext(msg1))
	if _, err := ApplyMessage(evm2, msg1, new(GasPool).AddGas(blockCtx.GasLimit)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overflow tx error = %v, want %v", err, ErrInsufficientFunds)
	}
	if got := sdb.GetBalance(sender); got.Cmp(balanceBeforeOverflow) != 0 {
		t.Fatalf("sender balance changed by a failed, excluded tx: got %s, want unchanged %s", got, balanceBeforeOverflow)
	}
	if got := sdb.GetBalance(recipient); got.Cmp(uint256.MustFromBig(value)) != 0 {
		t.Fatalf("recipient balance=%s, want %s (only the within-quota tx's value landed)", got, value)
	}
}
