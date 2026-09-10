//go:build integration
// +build integration

package bor

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/params"
)

// reservedPoolBalanceQuota is the client's per-block gas quota, sized to fit
// exactly one of the 21000-gas transfers below. Any second transfer from the
// same sender that's simultaneously pending competes for the same quota and
// overflows for that block.
const reservedPoolBalanceQuota = 21_000

// TestReservedBlockspacePoolAdmitsValueOnlyBalance is POS-3671's end-to-end
// validation against the real registry contract and a live two-node
// network. A registered reserved sender is funded with only enough balance
// to cover a handful of call values, never gas*feeCap, and submits several
// fallback-fee transactions at once against a client whose quota fits
// exactly one of them per block.
//
// Before this task, the pool priced every sender at full cost (value +
// gas*feeCap) for every balance check, so these transactions would either be
// rejected outright at admission or, if somehow admitted, evicted the moment
// a head event revalidated pending balances — regardless of whether they
// would eventually get their quota turn. With the reserved-aware
// EffectiveCost wired through admission and the pending-side balance
// Filter, all of them are admitted and none are ever dropped while they
// wait: the quota lets exactly one per block execute fee-free, and the
// pool's value-only pricing (this task) keeps the rest pending — never
// evicted for lacking gas balance — until it's their turn. Since the
// sender's balance can never cover a real EIP-1559 payment, every one of
// them landing in the canonical chain at all is itself proof it went
// through the fee-free reserved path. A second node that only verifies in
// this window accepts the same chain, preserving produce/verify parity.
//
// Run with: go test -tags=integration -run TestReservedBlockspacePoolAdmitsValueOnlyBalance ./tests/bor/
func TestReservedBlockspacePoolAdmitsValueOnlyBalance(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 10)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	reservedKey := faucets[0]
	reservedAddr := crypto.PubkeyToAddress(reservedKey.PublicKey)
	ownerKey := faucets[2]
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)

	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	setupABI, err := abi.JSON(strings.NewReader(reservedRegistrySetupABI))
	if err != nil {
		t.Fatal(err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	// Reserved fork at block 5 (Cancun is active from block 3, so the reserved
	// header fields encode in the post-Cancun BlockExtraData format).
	reservedFork := uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	// Giugliano must not activate after reserved blockspace (its base-fee params
	// are earlier optional BlockExtraData fields than ReservedGasUsed); co-activate
	// them so post-fork blocks stamp all the optional fields together.
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	// The registry runtime bytecode (solc 0.8.33) uses PUSH0, a Shanghai opcode.
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}

	const (
		numTxs = 4
		value  = 100
	)
	// Enough POL for numTxs call values, nowhere near 21000 gas at any
	// realistic fee cap (~1.05e15 wei at the 50 gwei fee cap used below).
	valueOnlyBalance := big.NewInt(numTxs * value * 10) // generous headroom, still dwarfed by gas*feeCap
	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000)
	genesis.Alloc[reservedAddr] = types.Account{Balance: new(big.Int).Set(valueOnlyBalance)}
	genesis.Alloc[ownerAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}

	stacks, nodes, _ := setupMiner(t, 2, genesis)
	defer func() {
		for _, stack := range stacks {
			stack.Close()
		}
	}()
	for _, node := range nodes {
		if err := node.StartMining(); err != nil {
			t.Fatal("start mining:", err)
		}
	}

	waitForBlock := func(target uint64) {
		t.Helper()
		deadline := time.After(120 * time.Second)
		for {
			if nodes[0].BlockChain().CurrentBlock().Number.Uint64() >= target {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("timeout waiting for block %d (at %d)", target, nodes[0].BlockChain().CurrentBlock().Number.Uint64())
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	// waitMined blocks until tx is found in a canonical block and returns it.
	waitMined := func(txHash common.Hash, what string) *types.Block {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			head := nodes[0].BlockChain().CurrentBlock().Number.Uint64()
			for n := uint64(0); n <= head; n++ {
				blk := nodes[0].BlockChain().GetBlockByNumber(n)
				if blk == nil {
					continue
				}
				for _, tx := range blk.Transactions() {
					if tx.Hash() == txHash {
						return blk
					}
				}
			}
			select {
			case <-deadline:
				t.Fatalf("timeout waiting for %s to be mined", what)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	signer := types.LatestSigner(genesis.Config)

	// Seed the registry exactly as the sibling reserved-blockspace tests do:
	// initialize() claims ownership, createClient() registers reservedAddr as
	// the sole whitelisted address of a client whose quota fits exactly one
	// 21000-gas transfer.
	sendOwnerTx := func(nonce uint64, data []byte) *types.Transaction {
		t.Helper()
		tx, err := types.SignNewTx(ownerKey, signer, &types.DynamicFeeTx{
			ChainID:   genesis.Config.ChainID,
			Nonce:     nonce,
			GasTipCap: big.NewInt(30_000_000_000),
			GasFeeCap: big.NewInt(100_000_000_000),
			Gas:       1_000_000,
			To:        &registryAddr,
			Value:     big.NewInt(0),
			Data:      data,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := nodes[0].APIBackend.SendTx(context.Background(), tx); err != nil {
			t.Fatalf("registry setup tx rejected: %v", err)
		}
		return tx
	}

	initData, err := setupABI.Pack("initialize", ownerAddr, uint64(8_000_000), uint64(5_000_000))
	if err != nil {
		t.Fatal(err)
	}
	createData, err := setupABI.Pack("createClient",
		ownerAddr, uint64(reservedPoolBalanceQuota), uint8(0), uint64(0), "pool-balance", []common.Address{reservedAddr})
	if err != nil {
		t.Fatal(err)
	}

	// London activates at block 1 in this genesis; the dynamic-fee setup txs
	// are rejected before then ("pool not yet in London").
	waitForBlock(2)

	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	createTx := sendOwnerTx(oNonce+1, createData)
	seedBlock := waitMined(createTx.Hash(), "createClient")
	t.Logf("registry seeded: createClient mined in block %d", seedBlock.NumberU64())

	// Give the registry state a few confirmations past both seeding and the
	// reserved fork before submitting, so the per-head snapshot both nodes
	// build for the next block already sees the client.
	waitForBlock(seedBlock.NumberU64() + 3)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	// Every transaction carries a real (fallback) fee: gas*feeCap alone
	// (21000 * 50 gwei ≈ 1.05e15 wei) dwarfs the sender's total balance.
	// Only the reserved waiver (within quota, execution-side) or the pool's
	// value-only pricing (overflow, this task) lets any of them be admitted
	// and stay in the pool at all.
	txs := make([]*types.Transaction, numTxs)
	for i := range txs {
		tx, err := types.SignNewTx(reservedKey, signer, &types.DynamicFeeTx{
			ChainID:   genesis.Config.ChainID,
			Nonce:     uint64(i),
			GasTipCap: big.NewInt(1_000_000_000),
			GasFeeCap: big.NewInt(50_000_000_000),
			Gas:       reservedPoolBalanceQuota,
			To:        &recipient,
			Value:     big.NewInt(value),
		})
		if err != nil {
			t.Fatal(err)
		}
		txs[i] = tx
	}

	// Submit the whole sequence atomically to each node's pool so all
	// numTxs nonces are simultaneously pending before the next block is
	// built, guaranteeing genuine per-block quota contention rather than
	// each nonce quietly getting its own turn one node-restart apart.
	for _, node := range nodes {
		for i, err := range node.TxPool().Add(txs, true) {
			if err != nil && !errors.Is(err, txpool.ErrAlreadyKnown) {
				t.Fatalf("value-only-balance fallback-fee tx %d rejected by pool: %v", i, err)
			}
		}
	}

	// minedIn reports, for each tx, the block it's currently canonical in on
	// node0 (nil if not yet mined there).
	minedIn := func() []*types.Block {
		head := nodes[0].BlockChain().CurrentBlock().Number.Uint64()
		found := make([]*types.Block, len(txs))
		remaining := len(txs)
		for n := seedBlock.NumberU64(); n <= head && remaining > 0; n++ {
			blk := nodes[0].BlockChain().GetBlockByNumber(n)
			if blk == nil {
				continue
			}
			for _, btx := range blk.Transactions() {
				for i, want := range txs {
					if found[i] == nil && btx.Hash() == want.Hash() {
						found[i] = blk
						remaining--
					}
				}
			}
		}
		return found
	}

	// minedOnNode reports whether txHash is canonical on node's own chain.
	// The producing node drops a tx from its pool the moment its own head
	// includes it, which can be several hundred ms before the other node
	// imports that block, so "missing from the pool" is only meaningful
	// against the same node's chain, not node0's.
	minedOnNode := func(node *eth.Ethereum, txHash common.Hash) bool {
		head := node.BlockChain().CurrentBlock().Number.Uint64()
		for n := seedBlock.NumberU64(); n <= head; n++ {
			blk := node.BlockChain().GetBlockByNumber(n)
			if blk == nil {
				continue
			}
			for _, btx := range blk.Transactions() {
				if btx.Hash() == txHash {
					return true
				}
			}
		}
		return false
	}

	// Poll until every tx is mined. At every step, any tx not yet mined must
	// still be sitting in both pools — the invariant this task guarantees:
	// an overflowing (quota-contended) fallback-fee tx from a value-only
	// balance is never dropped while it waits its turn. A tx can transiently
	// be neither pending in a node's pool nor canonical there (tip-level fork
	// resolution reinjects it a moment after the losing block is unwound), so
	// a drop only counts once it persists across consecutive polls; a genuine
	// eviction is permanent and always crosses the threshold.
	deadline := time.After(150 * time.Second)
	var lastMinedBlock uint64
	misses := make([][]int, len(nodes))
	for ni := range misses {
		misses[ni] = make([]int, len(txs))
	}
	const maxConsecutiveMisses = 25 // 5s of 200ms polls
	for {
		found := minedIn()
		allMined := true
		for i, blk := range found {
			if blk == nil {
				allMined = false
				for ni, node := range nodes {
					pending := node.TxPool().Has(txs[i].Hash()) &&
						node.TxPool().Status(txs[i].Hash()) == txpool.TxStatusPending
					if pending || minedOnNode(node, txs[i].Hash()) {
						misses[ni][i] = 0
						continue
					}
					misses[ni][i]++
					if misses[ni][i] >= maxConsecutiveMisses {
						t.Fatalf("node %d: tx %d (nonce %d) was dropped from the pool (status %v) before being mined",
							ni, i, i, node.TxPool().Status(txs[i].Hash()))
					}
				}
			} else if blk.NumberU64() > lastMinedBlock {
				lastMinedBlock = blk.NumberU64()
			}
		}
		if allMined {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for all %d value-only-balance fallback-fee txs to be mined", numTxs)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("all %d value-only-balance fallback-fee txs mined by block %d", numTxs, lastMinedBlock)

	// Let a few confirmations settle so any tip-level fork resolves, then
	// compare canonical chains for produce/verify parity: the second node
	// (which did not exclusively produce this window) must agree.
	settleTarget := lastMinedBlock + 3
	waitForBlock(settleTarget)
	deadline = time.After(60 * time.Second)
	for {
		h0 := nodes[0].BlockChain().GetBlockByNumber(settleTarget)
		h1 := nodes[1].BlockChain().GetBlockByNumber(settleTarget)
		if h0 != nil && h1 != nil && h0.Hash() == h1.Hash() {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("nodes never converged on block %d", settleTarget)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Every one of the numTxs transactions landing in the canonical chain at
	// all is proof each went through the fee-free reserved path: the
	// sender's balance could never cover a real EIP-1559 payment. Confirm it
	// directly too: the total balance decrease is exactly numTxs*value, no
	// gas was ever debited.
	tip := nodes[0].BlockChain().CurrentBlock()
	st, err := nodes[0].BlockChain().StateAt(tip.Root)
	if err != nil {
		t.Fatal(err)
	}
	got := st.GetBalance(reservedAddr).ToBig()
	want := new(big.Int).Sub(valueOnlyBalance, big.NewInt(numTxs*value))
	if got.Cmp(want) != 0 {
		t.Fatalf("reserved sender balance = %s, want %s (call values only, no gas ever debited)", got, want)
	}
}
