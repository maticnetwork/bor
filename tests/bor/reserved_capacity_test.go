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
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/params"
)

// reservedRegistryGovernanceABI is the write surface this file drives beyond
// what reservedRegistrySetupABI (reserved_blockspace_test.go) covers:
// governance calls that change an already-initialized registry's limits and
// an existing client's quota, exercised mid-run against a live two-node
// network (POS-3669 §2.5 governance-transition cases).
const reservedRegistryGovernanceABI = `[
 {"inputs":[{"name":"initialOwner","type":"address"},{"name":"maxTotalGas","type":"uint64"},{"name":"maxClientGas","type":"uint64"}],"name":"initialize","outputs":[],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"name":"admin","type":"address"},{"name":"gasQuota","type":"uint64"},{"name":"feeMode","type":"uint8"},{"name":"effectiveFrom","type":"uint64"},{"name":"metadata","type":"string"},{"name":"addresses","type":"address[]"}],"name":"createClient","outputs":[{"name":"clientId","type":"uint256"}],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"name":"maxTotalGas","type":"uint64"},{"name":"maxClientGas","type":"uint64"}],"name":"setLimits","outputs":[],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"name":"clientId","type":"uint256"},{"name":"newQuota","type":"uint64"}],"name":"setClientQuota","outputs":[],"stateMutability":"nonpayable","type":"function"}
]`

// waitForBorBlockHeight polls node0's chain head until it reaches target.
func waitForBorBlockHeight(t *testing.T, nodes []*eth.Ethereum, target uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
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

// waitForBorTxMined blocks until tx is found in a canonical block on node0
// and returns that block.
func waitForBorTxMined(t *testing.T, nodes []*eth.Ethereum, txHash common.Hash, what string) *types.Block {
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

// reservedCapacityAt reads the reserved capacity stamped in the canonical
// header at height n on the given node, failing the test if the block or the
// field is missing.
func reservedCapacityAt(t *testing.T, n *eth.Ethereum, cfg *params.ChainConfig, height uint64) uint64 {
	t.Helper()
	blk := n.BlockChain().GetBlockByNumber(height)
	if blk == nil {
		t.Fatalf("block %d not found", height)
	}
	capacity := blk.Header().GetReservedCapacity(cfg)
	if capacity == nil {
		t.Fatalf("block %d missing ReservedCapacity header field", height)
	}
	return *capacity
}

// newReservedCapacityGenesis builds the shared genesis for this file's tests:
// a fresh 2-validator network with the reserved-blockspace fork activated
// early, the registry deployed with empty storage (governance seeds it at
// runtime, mirroring production), and Giugliano/Shanghai co-activated as the
// fork ordering and the registry's PUSH0 opcode require. Mirrors the setup in
// reserved_blockspace_test.go.
func newReservedCapacityGenesis(t *testing.T, faucets []*ecdsa.PrivateKey, ownerAddr common.Address) (*core.Genesis, common.Address) {
	t.Helper()
	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	const reservedFork = uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}
	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000)
	genesis.Alloc[ownerAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
	return genesis, registryAddr
}

// TestReservedCapacityGovernanceTransition_FutureEffectiveFrom is the
// governance-transition case (a) from POS-3669 §2.5: a client created with a
// future effectiveFrom. The stamped capacity must exclude it until the
// boundary block, then include it exactly at the crossing block even though
// no registry transaction lands in that block, with both nodes (producer and
// verifier across the run) agreeing throughout.
//
// Run with: go test -tags=integration -run TestReservedCapacityGovernanceTransition_FutureEffectiveFrom ./tests/bor/
func TestReservedCapacityGovernanceTransition_FutureEffectiveFrom(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 4)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	ownerKey := faucets[0]
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)
	clientAddr := crypto.PubkeyToAddress(faucets[1].PublicKey)

	govABI, err := abi.JSON(strings.NewReader(reservedRegistryGovernanceABI))
	if err != nil {
		t.Fatal(err)
	}
	genesis, registryAddr := newReservedCapacityGenesis(t, faucets, ownerAddr)

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

	waitForBorBlockHeight(t, nodes, 2, 60*time.Second)

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
			t.Fatalf("governance tx rejected: %v", err)
		}
		return tx
	}

	const clientQuota = uint64(4_000_000)
	const effectiveFrom = uint64(20) // comfortably past createClient's inclusion block

	initData, err := govABI.Pack("initialize", ownerAddr, uint64(8_000_000), uint64(5_000_000))
	if err != nil {
		t.Fatal(err)
	}
	createData, err := govABI.Pack("createClient",
		ownerAddr, clientQuota, uint8(0), effectiveFrom, "future-client", []common.Address{clientAddr})
	if err != nil {
		t.Fatal(err)
	}

	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	createTx := sendOwnerTx(oNonce+1, createData)
	seedBlock := waitForBorTxMined(t, nodes, createTx.Hash(), "createClient")
	if seedBlock.NumberU64() >= effectiveFrom-2 {
		t.Fatalf("createClient seeded too late (block %d) to observe the pre-boundary window before block %d", seedBlock.NumberU64(), effectiveFrom)
	}
	t.Logf("createClient (effectiveFrom=%d) mined in block %d", effectiveFrom, seedBlock.NumberU64())

	// Settle a few blocks past the boundary on both nodes.
	waitForBorBlockHeight(t, nodes, effectiveFrom+3, 120*time.Second)

	// Both nodes must present an identical canonical chain over the whole
	// window (produce/verify parity across the boundary).
	for h := seedBlock.NumberU64() + 1; h <= effectiveFrom+3; h++ {
		b0 := nodes[0].BlockChain().GetBlockByNumber(h)
		b1 := nodes[1].BlockChain().GetBlockByNumber(h)
		if b0 == nil || b1 == nil {
			t.Fatalf("block %d missing on a node (node0=%v node1=%v)", h, b0 != nil, b1 != nil)
		}
		if b0.Hash() != b1.Hash() {
			t.Fatalf("chains diverge at block %d: node0=%s node1=%s", h, b0.Hash(), b1.Hash())
		}
	}

	// Before the boundary: the future client's quota is part of the raw
	// registry total (createClient bumped it immediately) but not yet part
	// of the effective set, so the stamped capacity is 0.
	for h := seedBlock.NumberU64() + 1; h < effectiveFrom; h++ {
		for _, n := range nodes {
			if got := reservedCapacityAt(t, n, genesis.Config, h); got != 0 {
				t.Fatalf("block %d capacity = %d, want 0 (client not yet effective)", h, got)
			}
		}
	}

	// At and after the boundary: the client is effective even though no
	// registry transaction landed in the crossing block itself.
	for h := effectiveFrom; h <= effectiveFrom+3; h++ {
		for i, n := range nodes {
			if got := reservedCapacityAt(t, n, genesis.Config, h); got != clientQuota {
				t.Fatalf("node%d block %d capacity = %d, want %d (client now effective)", i, h, got, clientQuota)
			}
		}
	}
}

// TestReservedCapacityLiveness_ExceedsGasLimit is the governance-transition
// case (b) from POS-3669 §2.5: quotas raised so effective capacity meets or
// exceeds the block gas limit mid-run. Blocks must keep producing, headers
// must carry the exact over-limit value (no silent clamping), the base fee
// must price at the reserved-aware target's full-target fallback, and the
// chain must recover (continue producing/verifying) once capacity drops back
// below the limit.
//
// Run with: go test -tags=integration -run TestReservedCapacityLiveness_ExceedsGasLimit ./tests/bor/
func TestReservedCapacityLiveness_ExceedsGasLimit(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 4)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	ownerKey := faucets[0]
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)
	clientAddr := crypto.PubkeyToAddress(faucets[1].PublicKey)

	govABI, err := abi.JSON(strings.NewReader(reservedRegistryGovernanceABI))
	if err != nil {
		t.Fatal(err)
	}
	genesis, registryAddr := newReservedCapacityGenesis(t, faucets, ownerAddr)

	// genesis_2val.json sets gasLimit = 0x989680 = 10_000_000.
	const gasLimit = uint64(10_000_000)
	const clientID = 1 // first (and only) client created below
	const initialQuota = uint64(4_000_000)
	const overLimitQuota = uint64(12_000_000) // > gasLimit
	const recoveredQuota = uint64(3_000_000)

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

	waitForBorBlockHeight(t, nodes, 2, 60*time.Second)

	signer := types.LatestSigner(genesis.Config)
	nextNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx := func(data []byte) *types.Transaction {
		t.Helper()
		tx, err := types.SignNewTx(ownerKey, signer, &types.DynamicFeeTx{
			ChainID:   genesis.Config.ChainID,
			Nonce:     nextNonce,
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
			t.Fatalf("governance tx rejected: %v", err)
		}
		nextNonce++
		return tx
	}

	initData, err := govABI.Pack("initialize", ownerAddr, uint64(8_000_000), uint64(5_000_000))
	if err != nil {
		t.Fatal(err)
	}
	createData, err := govABI.Pack("createClient",
		ownerAddr, initialQuota, uint8(0), uint64(0), "liveness-client", []common.Address{clientAddr})
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(initData)
	createTx := sendOwnerTx(createData)
	seedBlock := waitForBorTxMined(t, nodes, createTx.Hash(), "createClient")

	// Baseline: capacity reflects the initial (under-limit) quota once the
	// registry state has propagated into the next block's parent snapshot.
	waitForBorBlockHeight(t, nodes, seedBlock.NumberU64()+2, 60*time.Second)
	if got := reservedCapacityAt(t, nodes[0], genesis.Config, seedBlock.NumberU64()+2); got != initialQuota {
		t.Fatalf("baseline capacity = %d, want %d", got, initialQuota)
	}

	// Raise limits then the client's quota above the block gas limit. Two
	// governance txs: setLimits must land before setClientQuota can validate
	// the raised quota against it.
	setLimitsData, err := govABI.Pack("setLimits", overLimitQuota+1_000_000, overLimitQuota)
	if err != nil {
		t.Fatal(err)
	}
	setQuotaUpData, err := govABI.Pack("setClientQuota", big.NewInt(clientID), overLimitQuota)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(setLimitsData)
	raiseTx := sendOwnerTx(setQuotaUpData)
	raiseBlock := waitForBorTxMined(t, nodes, raiseTx.Hash(), "setClientQuota (raise)")

	// Give the chain room to produce several more blocks past the raise —
	// the liveness assertion is that nothing halts.
	target := raiseBlock.NumberU64() + 4
	waitForBorBlockHeight(t, nodes, target, 120*time.Second)

	for h := raiseBlock.NumberU64() + 2; h <= target; h++ {
		for i, n := range nodes {
			got := reservedCapacityAt(t, n, genesis.Config, h)
			if got != overLimitQuota {
				t.Fatalf("node%d block %d capacity = %d, want exact over-limit value %d", i, h, got, overLimitQuota)
			}
			if got < gasLimit {
				t.Fatalf("node%d block %d capacity %d unexpectedly below gas limit %d", i, h, got, gasLimit)
			}
		}
		// Base fee must follow the reserved-aware target's full-target
		// fallback (capacity >= parent.GasLimit): recompute it independently
		// from the parent header and compare against what was actually mined.
		child := nodes[0].BlockChain().GetBlockByNumber(h)
		parent := nodes[0].BlockChain().GetBlockByNumber(h - 1)
		if child == nil || parent == nil {
			t.Fatalf("missing block for base-fee check at height %d", h)
		}
		want := eip1559.CalcBaseFee(genesis.Config, parent.Header())
		if child.BaseFee().Cmp(want) != 0 {
			t.Fatalf("block %d baseFee = %s, want %s (full-target fallback recomputed from parent %d)",
				h, child.BaseFee(), want, h-1)
		}
	}

	// Nodes still agree throughout the over-limit window.
	for h := raiseBlock.NumberU64(); h <= target; h++ {
		b0 := nodes[0].BlockChain().GetBlockByNumber(h)
		b1 := nodes[1].BlockChain().GetBlockByNumber(h)
		if b0 == nil || b1 == nil || b0.Hash() != b1.Hash() {
			t.Fatalf("chains diverge at block %d during the over-limit window", h)
		}
	}

	// Recovery: drop the quota back below the gas limit and confirm the
	// chain keeps producing and verifying with the reduced value.
	setQuotaDownData, err := govABI.Pack("setClientQuota", big.NewInt(clientID), recoveredQuota)
	if err != nil {
		t.Fatal(err)
	}
	dropTx := sendOwnerTx(setQuotaDownData)
	dropBlock := waitForBorTxMined(t, nodes, dropTx.Hash(), "setClientQuota (recover)")

	recoverTarget := dropBlock.NumberU64() + 3
	waitForBorBlockHeight(t, nodes, recoverTarget, 60*time.Second)

	for h := dropBlock.NumberU64() + 2; h <= recoverTarget; h++ {
		for i, n := range nodes {
			got := reservedCapacityAt(t, n, genesis.Config, h)
			if got != recoveredQuota {
				t.Fatalf("node%d block %d capacity after recovery = %d, want %d", i, h, got, recoveredQuota)
			}
		}
		b0 := nodes[0].BlockChain().GetBlockByNumber(h)
		b1 := nodes[1].BlockChain().GetBlockByNumber(h)
		if b0 == nil || b1 == nil || b0.Hash() != b1.Hash() {
			t.Fatalf("chains diverge at block %d after recovery", h)
		}
	}
}
