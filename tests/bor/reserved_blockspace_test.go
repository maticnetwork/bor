//go:build integration
// +build integration

package bor

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/params"
)

// reservedRegistrySetupABI is the minimal setter surface the test drives to seed
// the genesis-deployed registry: claim ownership, then register one client with a
// whitelisted address. Bor's own embedded ABI is read-only, so the writes live here.
const reservedRegistrySetupABI = `[
 {"inputs":[{"name":"initialOwner","type":"address"},{"name":"maxTotalGas","type":"uint64"},{"name":"maxClientGas","type":"uint64"}],"name":"initialize","outputs":[],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"name":"admin","type":"address"},{"name":"gasQuota","type":"uint64"},{"name":"feeMode","type":"uint8"},{"name":"effectiveFrom","type":"uint64"},{"name":"metadata","type":"string"},{"name":"addresses","type":"address[]"}],"name":"createClient","outputs":[{"name":"clientId","type":"uint256"}],"stateMutability":"nonpayable","type":"function"}
]`

// TestReservedBlockspaceZeroFeeProduction is the end-to-end (in-process devnet)
// validation of the reserved-blockspace feature against the real registry contract.
// A two-node bor network produces and cross-verifies blocks; the registry contract
// is deployed in genesis and seeded at runtime via initialize()+createClient() so a
// client's address becomes reserved through actual contract state — the same source
// (registry → Go reader → per-block snapshot) the txpool and EVM classify from in
// production. A zero-fee tx from that registered address is admitted to the pool,
// mined past the ReservedBlockspace fork with the reserved header fields set by
// the real Prepare path, and executes fee-free so the sender pays only its call
// value with no gas debit. An identical zero-fee tx from an address absent from
// the registry is rejected at admission.
//
// Run with: go test -tags=integration -run TestReservedBlockspaceZeroFeeProduction ./tests/bor/
func TestReservedBlockspaceZeroFeeProduction(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 10)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	reservedAddr := crypto.PubkeyToAddress(faucets[0].PublicKey)
	nonReservedAddr := crypto.PubkeyToAddress(faucets[1].PublicKey)
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
	// This genesis activates Cancun at 3 but omits Shanghai; activate it at 2 so
	// the contract is callable before the reserved fork.
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	// Deploy the registry contract in genesis (empty storage — ownership and
	// clients are established at runtime, exactly as on a real network) and point
	// the chain config at it so the engine wires the reader into txpool/EVM/miner.
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}

	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000) // 1 ETH
	genesis.Alloc[reservedAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
	genesis.Alloc[nonReservedAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
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

	// Seed the registry: initialize() claims ownership, createClient() registers
	// reservedAddr as the sole whitelisted address of a fee-free client. Both are
	// ordinary fee-paying txs from the owner; they execute in nonce order.
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
		// Submit to the producer only; p2p propagates to the peer.
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
		ownerAddr, uint64(5_000_000), uint8(0), uint64(0), "e2e", []common.Address{reservedAddr})
	if err != nil {
		t.Fatal(err)
	}

	// London activates at block 1 in this genesis; the dynamic-fee setup txs are
	// rejected before then ("pool not yet in London").
	waitForBlock(2)

	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	createTx := sendOwnerTx(oNonce+1, createData)
	seedBlock := waitMined(createTx.Hash(), "createClient")
	t.Logf("registry seeded: createClient mined in block %d", seedBlock.NumberU64())

	// Land the reserved tx inside node0's primary-producer window. With sprint=8
	// and two validators, node0 is the in-turn producer for blocks 1-8 and 17-24
	// while node1 produces 9-16 (see bor_test.go). Submitting at the start of the
	// 17-24 window lets node0 mine the tx canonically with room to spare, so the
	// chain doesn't reorg it out at the 9/17 producer handover. The parent state by
	// then long carries the client (createClient mined at block ~4), and the block
	// is well past the reserved fork (5).
	if seedBlock.NumberU64() >= 16 {
		t.Fatalf("registry seeded too late (block %d) for the 17-24 producer window", seedBlock.NumberU64())
	}
	waitForBlock(17)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	zeroFeeTx := func(from *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(from, signer, &types.DynamicFeeTx{
			ChainID:   genesis.Config.ChainID,
			Nonce:     nonce,
			GasTipCap: big.NewInt(0),
			GasFeeCap: big.NewInt(0),
			Gas:       21000,
			To:        &recipient,
			Value:     big.NewInt(100),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	// Reserved sender: zero-fee tx must be accepted by the pool.
	rNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), reservedAddr)
	if err != nil {
		t.Fatal(err)
	}
	rtx := zeroFeeTx(faucets[0], rNonce)
	for _, node := range nodes {
		// Tolerate "already known" from the peer that received it via p2p — what
		// matters is that admission classified the reserved sender, not rejected it.
		if err := node.APIBackend.SendTx(context.Background(), rtx); err != nil &&
			!strings.Contains(err.Error(), "already known") {
			t.Fatalf("reserved zero-fee tx rejected by pool: %v", err)
		}
	}

	// Non-reserved sender: identical zero-fee tx must be rejected at admission.
	nrNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), nonReservedAddr)
	if err != nil {
		t.Fatal(err)
	}
	otx := zeroFeeTx(faucets[1], nrNonce)
	if err := nodes[0].APIBackend.SendTx(context.Background(), otx); err == nil {
		t.Fatal("non-reserved zero-fee tx should be rejected at admission, but was accepted")
	}

	firstSeen := waitMined(rtx.Hash(), "reserved zero-fee tx")
	t.Logf("reserved zero-fee tx first seen in block %d", firstSeen.NumberU64())

	// Let the tip settle: both miners may briefly fork at the tip (the peer can
	// build a competing block before the reserved tx propagates). Wait for several
	// confirmations so fork choice converges, then read the tx's settled canonical
	// block. A real classification split would never converge — the validator would
	// reject the producer's block as a bad block — so convergence here is itself the
	// consensus-parity assertion.
	canonicalTxBlock := func(n *eth.Ethereum) *types.Block {
		head := n.BlockChain().CurrentBlock().Number.Uint64()
		for h := firstSeen.NumberU64(); h <= head; h++ {
			blk := n.BlockChain().GetBlockByNumber(h)
			if blk == nil {
				continue
			}
			for _, tx := range blk.Transactions() {
				if tx.Hash() == rtx.Hash() {
					return blk
				}
			}
		}
		return nil
	}

	var includedBlock, peerBlock *types.Block
	settleDeadline := time.After(60 * time.Second)
	for {
		includedBlock = canonicalTxBlock(nodes[0])
		peerBlock = canonicalTxBlock(nodes[1])
		if includedBlock != nil && peerBlock != nil &&
			includedBlock.Hash() == peerBlock.Hash() &&
			nodes[1].BlockChain().CurrentBlock().Number.Uint64() >= includedBlock.NumberU64()+3 {
			break
		}
		select {
		case <-settleDeadline:
			h0 := nodes[0].BlockChain().CurrentBlock()
			h1 := nodes[1].BlockChain().CurrentBlock()
			t.Logf("node0 head=%d %s | node1 head=%d %s", h0.Number.Uint64(), h0.Hash(), h1.Number.Uint64(), h1.Hash())
			if includedBlock != nil {
				t.Logf("node0 tx-block=%d %s; node1 has it by hash: %v",
					includedBlock.NumberU64(), includedBlock.Hash(),
					nodes[1].BlockChain().GetBlockByHash(includedBlock.Hash()) != nil)
			}
			if peerBlock != nil {
				t.Logf("node1 tx-block=%d %s; node0 has it by hash: %v",
					peerBlock.NumberU64(), peerBlock.Hash(),
					nodes[0].BlockChain().GetBlockByHash(peerBlock.Hash()) != nil)
			}
			// Compare canonical hashes at each height up to the lower head to find
			// where (if anywhere) the chains forked.
			lo := h0.Number.Uint64()
			if h1.Number.Uint64() < lo {
				lo = h1.Number.Uint64()
			}
			for n := uint64(1); n <= lo; n++ {
				b0 := nodes[0].BlockChain().GetBlockByNumber(n)
				b1 := nodes[1].BlockChain().GetBlockByNumber(n)
				if b0 == nil || b1 == nil || b0.Hash() != b1.Hash() {
					t.Logf("chains first differ at height %d: node0=%v node1=%v", n, b0.Hash(), b1.Hash())
					break
				}
			}
			t.Fatalf("nodes never converged on the reserved tx block")
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("reserved zero-fee tx settled in canonical block %d on both nodes", includedBlock.NumberU64())

	// The producing block must carry the reserved header field (set by Prepare).
	if rg := includedBlock.Header().GetReservedGasUsed(genesis.Config); rg == nil {
		t.Fatalf("block %d missing reserved gas used header field", includedBlock.NumberU64())
	}

	// The reserved tx's receipt reports a zero effective gas price — downstream
	// consumers (RPC / explorers) see it as fee-free.
	if rc := findReceipt(t, nodes[0], rtx.Hash()); rc.EffectiveGasPrice != nil && rc.EffectiveGasPrice.Sign() != 0 {
		t.Fatalf("reserved tx effectiveGasPrice = %s, want 0", rc.EffectiveGasPrice)
	}

	// The reserved sender paid only its call value — no gas was debited — and both
	// nodes computed the same post-state for the block (identical hash above).
	state, err := nodes[0].BlockChain().StateAt(includedBlock.Root())
	if err != nil {
		t.Fatal(err)
	}
	got := state.GetBalance(reservedAddr).ToBig()
	want := new(big.Int).Sub(startBalance, big.NewInt(100))
	if got.Cmp(want) != 0 {
		t.Fatalf("reserved sender balance = %s, want %s (call value only, no gas debit)", got, want)
	}
}

// TestReservedBlockspaceQuotaOverflowPaysNormalFees is the quota-overflow
// (case B) end-to-end validation of the V5 invariant: a registered sender's
// transactions are fee-free only up to its per-client quota; beyond the quota
// they must pay normal fees. Two whitelisted senders share one client whose gas
// quota fits exactly one 21000-gas transaction. The reserved pass prefers the
// ascending-fee (zero-fee) transaction, so the zero-fee sender wins the quota
// and executes fee-free, while the fallback-fee sender overflows into the normal
// region and is charged normal EIP-1559 fees. Both nodes converge on the same
// block (the produce-vs-verify parity assertion: a verifier rederives the
// identical reserved/overflow split from the ordered body, so a classification
// mismatch would surface as a rejected block, never convergence).
//
// Run with: go test -tags=integration -run TestReservedBlockspaceQuotaOverflowPaysNormalFees ./tests/bor/
func TestReservedBlockspaceQuotaOverflowPaysNormalFees(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 10)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	freeKey := faucets[0]     // zero-fee sender: wins the quota
	overflowKey := faucets[1] // fallback-fee sender: overflows to normal
	freeAddr := crypto.PubkeyToAddress(freeKey.PublicKey)
	overflowAddr := crypto.PubkeyToAddress(overflowKey.PublicKey)
	ownerKey := faucets[2]
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)

	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	setupABI, err := abi.JSON(strings.NewReader(reservedRegistrySetupABI))
	if err != nil {
		t.Fatal(err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	reservedFork := uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	// Giugliano must not activate after reserved blockspace (its base-fee params
	// are earlier optional BlockExtraData fields than ReservedGasUsed); co-activate
	// them so post-fork blocks stamp all the optional fields together.
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}

	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000) // 1 ETH
	genesis.Alloc[freeAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
	genesis.Alloc[overflowAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
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
	// One client, quota = 21000 (exactly one simple transfer), two whitelisted
	// addresses. Only one of the two 21000-gas txs can be reserved.
	createData, err := setupABI.Pack("createClient",
		ownerAddr, uint64(21_000), uint8(0), uint64(0), "overflow", []common.Address{freeAddr, overflowAddr})
	if err != nil {
		t.Fatal(err)
	}

	waitForBlock(2)

	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	createTx := sendOwnerTx(oNonce+1, createData)
	seedBlock := waitMined(createTx.Hash(), "createClient")
	if seedBlock.NumberU64() >= 16 {
		t.Fatalf("registry seeded too late (block %d) for the 17-24 producer window", seedBlock.NumberU64())
	}
	waitForBlock(17)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	// Zero-fee tx from the free sender: below base fee, so only its reserved
	// classification admits it. It wins the scarce quota.
	freeTx, err := types.SignNewTx(freeKey, signer, &types.DynamicFeeTx{
		ChainID:   genesis.Config.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(0),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fallback-fee tx from the overflow sender: it carries a real fee, so when it
	// overflows the quota it is admitted to the normal region and pays EIP-1559
	// fees.
	overflowTx, err := types.SignNewTx(overflowKey, signer, &types.DynamicFeeTx{
		ChainID:   genesis.Config.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(50_000_000_000),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, node := range nodes {
		if err := node.APIBackend.SendTx(context.Background(), freeTx); err != nil &&
			!strings.Contains(err.Error(), "already known") {
			t.Fatalf("free zero-fee tx rejected by pool: %v", err)
		}
		if err := node.APIBackend.SendTx(context.Background(), overflowTx); err != nil &&
			!strings.Contains(err.Error(), "already known") {
			t.Fatalf("overflow fallback-fee tx rejected by pool: %v", err)
		}
	}

	freeBlock := waitMined(freeTx.Hash(), "free zero-fee tx")
	overflowBlock := waitMined(overflowTx.Hash(), "overflow fallback-fee tx")

	// Settle: wait for both nodes to agree on both tx blocks with confirmations,
	// so fork choice has converged. Non-convergence would mean a produce/verify
	// classification split.
	txBlockOn := func(n *eth.Ethereum, txHash common.Hash, from uint64) *types.Block {
		head := n.BlockChain().CurrentBlock().Number.Uint64()
		for h := from; h <= head; h++ {
			blk := n.BlockChain().GetBlockByNumber(h)
			if blk == nil {
				continue
			}
			for _, tx := range blk.Transactions() {
				if tx.Hash() == txHash {
					return blk
				}
			}
		}
		return nil
	}

	lowest := freeBlock.NumberU64()
	if overflowBlock.NumberU64() < lowest {
		lowest = overflowBlock.NumberU64()
	}

	var settledFree, settledOverflow *types.Block
	settleDeadline := time.After(60 * time.Second)
	for {
		f0 := txBlockOn(nodes[0], freeTx.Hash(), lowest)
		o0 := txBlockOn(nodes[0], overflowTx.Hash(), lowest)
		f1 := txBlockOn(nodes[1], freeTx.Hash(), lowest)
		o1 := txBlockOn(nodes[1], overflowTx.Hash(), lowest)
		if f0 != nil && o0 != nil && f1 != nil && o1 != nil &&
			f0.Hash() == f1.Hash() && o0.Hash() == o1.Hash() {
			highest := o0.NumberU64()
			if f0.NumberU64() > highest {
				highest = f0.NumberU64()
			}
			if nodes[1].BlockChain().CurrentBlock().Number.Uint64() >= highest+3 {
				settledFree, settledOverflow = f0, o0
				break
			}
		}
		select {
		case <-settleDeadline:
			t.Fatalf("nodes never converged on the reserved/overflow tx blocks")
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("free tx settled in block %d, overflow tx in block %d", settledFree.NumberU64(), settledOverflow.NumberU64())

	readState := func(b *types.Block) *state.StateDB {
		st, err := nodes[0].BlockChain().StateAt(b.Root())
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Free sender paid only its call value: reserved, fee-free.
	freeGot := readState(settledFree).GetBalance(freeAddr).ToBig()
	freeWant := new(big.Int).Sub(startBalance, big.NewInt(100))
	if freeGot.Cmp(freeWant) != 0 {
		t.Fatalf("free (reserved) sender balance = %s, want %s (call value only, no gas debit)", freeGot, freeWant)
	}

	// Overflow sender paid its call value plus normal EIP-1559 gas: value +
	// gasUsed*effectiveGasPrice, read from its receipt.
	receipts := nodes[0].BlockChain().GetReceiptsByHash(settledOverflow.Hash())
	var overflowReceipt *types.Receipt
	for _, r := range receipts {
		if r.TxHash == overflowTx.Hash() {
			overflowReceipt = r
			break
		}
	}
	if overflowReceipt == nil {
		t.Fatal("overflow tx receipt not found")
	}
	gasCost := new(big.Int).Mul(new(big.Int).SetUint64(overflowReceipt.GasUsed), overflowReceipt.EffectiveGasPrice)
	if gasCost.Sign() == 0 {
		t.Fatal("overflow tx paid zero gas — quota overflow was not charged normal fees (V5 violation)")
	}
	overflowGot := readState(settledOverflow).GetBalance(overflowAddr).ToBig()
	overflowWant := new(big.Int).Sub(new(big.Int).Sub(startBalance, big.NewInt(100)), gasCost)
	if overflowGot.Cmp(overflowWant) != 0 {
		t.Fatalf("overflow sender balance = %s, want %s (call value + normal gas %s)", overflowGot, overflowWant, gasCost)
	}
}

// TestReservedBlockspaceMultiClient is the multi-client (case C/D) end-to-end
// validation: two independent clients with different per-client quotas plus a
// non-registered sender, all producing in the same window. Client A (quota for
// one tx) sends one zero-fee tx → reserved. Client B (quota for two txs) sends
// two zero-fee txs → both reserved, then a third fallback-fee tx that overflows
// its quota → pays normal fees. A non-registered sender's zero-fee tx is
// rejected at admission. Per-client quotas are independent (client A being full
// does not affect client B), and both nodes converge on identical blocks
// (produce/verify parity across multiple interleaved clients).
//
// Run with: go test -tags=integration -run TestReservedBlockspaceMultiClient ./tests/bor/
func TestReservedBlockspaceMultiClient(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 10)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	aKey := faucets[0]  // client A: one reserved tx
	bKey := faucets[1]  // client B: two reserved + one overflow
	nrKey := faucets[3] // non-registered
	aAddr := crypto.PubkeyToAddress(aKey.PublicKey)
	bAddr := crypto.PubkeyToAddress(bKey.PublicKey)
	nrAddr := crypto.PubkeyToAddress(nrKey.PublicKey)
	ownerKey := faucets[2]
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)

	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	setupABI, err := abi.JSON(strings.NewReader(reservedRegistrySetupABI))
	if err != nil {
		t.Fatal(err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	reservedFork := uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	// Giugliano must not activate after reserved blockspace (its base-fee params
	// are earlier optional BlockExtraData fields than ReservedGasUsed); co-activate
	// them so post-fork blocks stamp all the optional fields together.
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{Balance: new(big.Int), Code: common.FromHex(params.ReservedBlockspaceRegistryCode)}

	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000)
	for _, a := range []common.Address{aAddr, bAddr, nrAddr, ownerAddr} {
		genesis.Alloc[a] = types.Account{Balance: new(big.Int).Set(startBalance)}
	}

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
				t.Fatalf("timeout waiting for block %d", target)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
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
				t.Fatalf("timeout waiting for %s", what)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	signer := types.LatestSigner(genesis.Config)
	sendOwnerTx := func(nonce uint64, data []byte) *types.Transaction {
		t.Helper()
		tx, err := types.SignNewTx(ownerKey, signer, &types.DynamicFeeTx{
			ChainID: genesis.Config.ChainID, Nonce: nonce,
			GasTipCap: big.NewInt(30_000_000_000), GasFeeCap: big.NewInt(100_000_000_000),
			Gas: 1_000_000, To: &registryAddr, Value: big.NewInt(0), Data: data,
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
	// Client A quota = 21000 (one transfer); client B quota = 42000 (two).
	createA, err := setupABI.Pack("createClient", ownerAddr, uint64(21_000), uint8(0), uint64(0), "A", []common.Address{aAddr})
	if err != nil {
		t.Fatal(err)
	}
	createB, err := setupABI.Pack("createClient", ownerAddr, uint64(42_000), uint8(0), uint64(0), "B", []common.Address{bAddr})
	if err != nil {
		t.Fatal(err)
	}

	waitForBlock(2)
	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	sendOwnerTx(oNonce+1, createA)
	lastSeed := sendOwnerTx(oNonce+2, createB)
	seedBlock := waitMined(lastSeed.Hash(), "createClient B")
	if seedBlock.NumberU64() >= 16 {
		t.Fatalf("registry seeded too late (block %d)", seedBlock.NumberU64())
	}
	waitForBlock(17)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	zeroFee := func(from *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(from, signer, &types.DynamicFeeTx{
			ChainID: genesis.Config.ChainID, Nonce: nonce,
			GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(0),
			Gas: 21000, To: &recipient, Value: big.NewInt(100),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	fallbackFee := func(from *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(from, signer, &types.DynamicFeeTx{
			ChainID: genesis.Config.ChainID, Nonce: nonce,
			GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(50_000_000_000),
			Gas: 21000, To: &recipient, Value: big.NewInt(100),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	aTx := zeroFee(aKey, 0)    // client A: reserved
	b0 := zeroFee(bKey, 0)     // client B: reserved
	b1 := zeroFee(bKey, 1)     // client B: reserved (fills 42000 quota)
	b2 := fallbackFee(bKey, 2) // client B: overflow → normal fees
	nrTx := zeroFee(nrKey, 0)  // non-registered → rejected

	for _, node := range nodes {
		for _, tx := range []*types.Transaction{aTx, b0, b1, b2} {
			if err := node.APIBackend.SendTx(context.Background(), tx); err != nil &&
				!strings.Contains(err.Error(), "already known") {
				t.Fatalf("reserved-eligible tx rejected: %v", err)
			}
		}
	}
	if err := nodes[0].APIBackend.SendTx(context.Background(), nrTx); err == nil {
		t.Fatal("non-registered zero-fee tx should be rejected at admission")
	}

	// Wait for all four to be mined, then let the tip settle with confirmations.
	var maxBlk uint64
	for _, tx := range []*types.Transaction{aTx, b0, b1, b2} {
		if bn := waitMined(tx.Hash(), "reserved-eligible tx").NumberU64(); bn > maxBlk {
			maxBlk = bn
		}
	}
	settleDeadline := time.After(60 * time.Second)
	for {
		h0 := nodes[0].BlockChain().CurrentBlock().Number.Uint64()
		h1 := nodes[1].BlockChain().CurrentBlock().Number.Uint64()
		if h0 >= maxBlk+3 && h1 >= maxBlk+3 &&
			nodes[0].BlockChain().GetBlockByNumber(maxBlk+2).Hash() == nodes[1].BlockChain().GetBlockByNumber(maxBlk+2).Hash() {
			break
		}
		select {
		case <-settleDeadline:
			t.Fatal("nodes never converged")
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Read settled canonical state (well past all tx blocks).
	confBlk := nodes[0].BlockChain().GetBlockByNumber(maxBlk + 2)
	st, err := nodes[0].BlockChain().StateAt(confBlk.Root())
	if err != nil {
		t.Fatal(err)
	}

	// Client A: one reserved tx, value only.
	if got, want := st.GetBalance(aAddr).ToBig(), new(big.Int).Sub(startBalance, big.NewInt(100)); got.Cmp(want) != 0 {
		t.Fatalf("client A balance = %s, want %s (reserved, value only)", got, want)
	}

	// Client B: two reserved (value only) + one overflow (value + normal gas).
	b2Receipt := findReceipt(t, nodes[0], b2.Hash())
	b2Gas := new(big.Int).Mul(new(big.Int).SetUint64(b2Receipt.GasUsed), b2Receipt.EffectiveGasPrice)
	if b2Gas.Sign() == 0 {
		t.Fatal("client B overflow tx paid zero gas (V5 violation)")
	}
	bWant := new(big.Int).Sub(startBalance, big.NewInt(300)) // three call values
	bWant.Sub(bWant, b2Gas)                                  // plus overflow gas
	if got := st.GetBalance(bAddr).ToBig(); got.Cmp(bWant) != 0 {
		t.Fatalf("client B balance = %s, want %s (2 reserved + 1 overflow gas %s)", got, bWant, b2Gas)
	}
}

// findReceipt returns the receipt for txHash from its canonical block.
func findReceipt(t *testing.T, n *eth.Ethereum, txHash common.Hash) *types.Receipt {
	t.Helper()
	head := n.BlockChain().CurrentBlock().Number.Uint64()
	for h := uint64(0); h <= head; h++ {
		blk := n.BlockChain().GetBlockByNumber(h)
		if blk == nil {
			continue
		}
		for _, tx := range blk.Transactions() {
			if tx.Hash() == txHash {
				for _, r := range n.BlockChain().GetReceiptsByHash(blk.Hash()) {
					if r.TxHash == txHash {
						return r
					}
				}
			}
		}
	}
	t.Fatalf("receipt for %s not found", txHash.Hex())
	return nil
}
