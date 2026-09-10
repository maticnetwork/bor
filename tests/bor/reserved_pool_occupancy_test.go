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
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// reservedPoolOccupancySetupABI is the registry setter surface this test
// drives, kept local rather than reusing reservedRegistrySetupABI
// (reserved_blockspace_test.go) because it additionally needs
// setClientActive to deterministically free reserved occupancy (see below).
const reservedPoolOccupancySetupABI = `[
 {"inputs":[{"name":"initialOwner","type":"address"},{"name":"maxTotalGas","type":"uint64"},{"name":"maxClientGas","type":"uint64"}],"name":"initialize","outputs":[],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"name":"admin","type":"address"},{"name":"gasQuota","type":"uint64"},{"name":"feeMode","type":"uint8"},{"name":"effectiveFrom","type":"uint64"},{"name":"metadata","type":"string"},{"name":"addresses","type":"address[]"}],"name":"createClient","outputs":[{"name":"clientId","type":"uint256"}],"stateMutability":"nonpayable","type":"function"},
 {"inputs":[{"name":"clientId","type":"uint256"},{"name":"active","type":"bool"}],"name":"setClientActive","outputs":[],"stateMutability":"nonpayable","type":"function"}
]`

// reservedPoolOccupancyGas is the gas limit carried by every reserved
// transaction below, matching a realistic transfer.
const reservedPoolOccupancyGas = 21_000

// TestReservedPoolOccupancy is POS-3681's end-to-end validation against the
// real registry contract, a live two-node network, and the pool's default
// occupancy cap (no config overrides): a reserved client whose whitelist is
// sized to threaten pool exhaustion floods zero-fee transactions from every
// one of its addresses. Before this task, that flood could grow until it
// consumed the entire pool — eviction-immune — leaving no admission room for
// any other sender.
//
// With the occupancy cap in place: the flood is bounded at
// ReservedMaxOccupancyPercent of the pool's own combined slot ceiling; an
// unrelated normal fee-paying sender's transaction is admitted and mined
// throughout, proving normal senders' headroom is genuinely held; and once
// the flooding client is deregistered (a realistic governance action, and
// the only deterministic way to free occupancy that this flood — entirely
// permanently-gapped, so never mined on its own — would not otherwise free),
// a different, still-whitelisted reserved sender's own transaction is
// admitted and goes on to be mined — the cap bounds occupancy, it does not
// permanently ban a reserved sender.
//
// Run with: go test -tags=integration -run TestReservedPoolOccupancy ./tests/bor/
func TestReservedPoolOccupancy(t *testing.T) {
	// wantCap is exact for the current defaults (GlobalSlots=5120,
	// GlobalQueue=1024, 50%): 3072. perFillerChainLen is just a convenient
	// per-address chunk size (reusing AccountQueue's value, though these
	// transactions end up pending rather than queued — see the flood
	// construction below for why that distinction matters here).
	wantCap := int((legacypool.DefaultConfig.GlobalSlots + legacypool.DefaultConfig.GlobalQueue) *
		legacypool.DefaultConfig.ReservedMaxOccupancyPercent / 100)
	perFillerChainLen := int(legacypool.DefaultConfig.AccountQueue)
	// One address beyond the exact ceiling so the flood itself overflows the
	// cap (some of its own transactions are rejected), rather than landing
	// exactly on the boundary with nothing left to reject.
	numFillers := (wantCap+perFillerChainLen-1)/perFillerChainLen + 1

	faucets := make([]*ecdsa.PrivateKey, 4)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	ownerKey := faucets[0]
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)
	normalKey := faucets[1]
	normalAddr := crypto.PubkeyToAddress(normalKey.PublicKey)

	fillerKeys := make([]*ecdsa.PrivateKey, numFillers)
	fillerAddrs := make([]common.Address, numFillers)
	for i := range fillerKeys {
		fillerKeys[i], _ = crypto.GenerateKey()
		fillerAddrs[i] = crypto.PubkeyToAddress(fillerKeys[i].PublicKey)
	}
	// probeAddr belongs to a *separate* client from the fillers, so
	// deregistering (deactivating) the flooding client below doesn't also
	// declassify probeAddr — the point is that a still-reserved sender
	// recovers headroom, not that every reserved sender does.
	probeKey, _ := crypto.GenerateKey()
	probeAddr := crypto.PubkeyToAddress(probeKey.PublicKey)

	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	setupABI, err := abi.JSON(strings.NewReader(reservedPoolOccupancySetupABI))
	if err != nil {
		t.Fatal(err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	// Reserved fork at block 5 (Cancun is active from block 3, so the reserved
	// header fields encode in the post-Cancun BlockExtraData format).
	reservedFork := uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	// The registry runtime bytecode (solc 0.8.33) uses PUSH0, a Shanghai opcode.
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}

	startBalance := new(big.Int).SetUint64(10_000_000_000_000_000_000)
	genesis.Alloc[ownerAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
	genesis.Alloc[normalAddr] = types.Account{Balance: new(big.Int).Set(startBalance)}
	// Every reserved-sender transaction below carries zero value and zero
	// fee: the pool prices a reserved sender's balance check on value alone
	// (EffectiveCost), so a real balance is never required. Each address
	// still needs a materialized account (a nonzero genesis balance is the
	// simplest way): an account the state has never touched at all reports
	// GetCodeHash as the zero hash rather than types.EmptyCodeHash, which
	// misclassifies it as a delegated account and caps it at one in-flight
	// transaction — unrelated to reserved-occupancy tracking, but every
	// address below needs more than one in-flight transaction to exercise it.
	for _, addr := range fillerAddrs {
		genesis.Alloc[addr] = types.Account{Balance: big.NewInt(1)}
	}
	genesis.Alloc[probeAddr] = types.Account{Balance: big.NewInt(1)}

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
		waitForBorBlockHeight(t, nodes, target, 120*time.Second)
	}

	// waitMined is waitForBorTxMined (reserved_capacity_test.go) plus a
	// revert check: this test's registry-seeding transactions carry
	// hand-picked gas limits, and a silent out-of-gas revert there would
	// otherwise surface as a much more confusing failure much later.
	waitMined := func(txHash common.Hash, what string) *types.Block {
		t.Helper()
		blk := waitForBorTxMined(t, nodes, txHash, what)
		for _, r := range nodes[0].BlockChain().GetReceiptsByHash(blk.Hash()) {
			if r.TxHash == txHash && r.Status != types.ReceiptStatusSuccessful {
				t.Fatalf("%s reverted in block %d", what, blk.NumberU64())
			}
		}
		return blk
	}

	signer := types.LatestSigner(genesis.Config)

	sendOwnerTx := func(nonce, gas uint64, data []byte) *types.Transaction {
		t.Helper()
		tx, err := types.SignNewTx(ownerKey, signer, &types.DynamicFeeTx{
			ChainID:   genesis.Config.ChainID,
			Nonce:     nonce,
			GasTipCap: big.NewInt(30_000_000_000),
			GasFeeCap: big.NewInt(100_000_000_000),
			Gas:       gas,
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

	// Two clients: the fillers (clientId 1, deactivated later) and the probe
	// sender alone (clientId 2, stays active throughout).
	initData, err := setupABI.Pack("initialize", ownerAddr, uint64(8_000_000), uint64(5_000_000))
	if err != nil {
		t.Fatal(err)
	}
	fillerClientData, err := setupABI.Pack("createClient",
		ownerAddr, uint64(reservedPoolOccupancyGas), uint8(0), uint64(0), "fillers", fillerAddrs)
	if err != nil {
		t.Fatal(err)
	}
	probeClientData, err := setupABI.Pack("createClient",
		ownerAddr, uint64(reservedPoolOccupancyGas), uint8(0), uint64(0), "probe", []common.Address{probeAddr})
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
	// Ordered so the fillers become clientId 1 (deactivated later) and the
	// probe sender becomes clientId 2 (stays active throughout).
	initTx := sendOwnerTx(oNonce, 200_000, initData)
	fillerClientTx := sendOwnerTx(oNonce+1, 9_000_000, fillerClientData)
	probeClientTx := sendOwnerTx(oNonce+2, 500_000, probeClientData)
	waitMined(initTx.Hash(), "initialize")
	waitMined(fillerClientTx.Hash(), "the fillers' createClient")
	seedBlock := waitMined(probeClientTx.Hash(), "the probe's createClient")
	t.Logf("registry seeded with %d filler addresses (clientId 1) and 1 probe address (clientId 2): mined by block %d", numFillers, seedBlock.NumberU64())

	// Give the registry state a few confirmations past both seeding and the
	// reserved fork before submitting, so the per-head snapshot both nodes
	// build for the next block already sees both clients.
	waitForBlock(seedBlock.NumberU64() + 3)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	buildReservedTx := func(key *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID:   genesis.Config.ChainID,
			Nonce:     nonce,
			GasTipCap: big.NewInt(0),
			GasFeeCap: big.NewInt(0),
			Gas:       reservedPoolOccupancyGas,
			To:        &recipient,
			Value:     big.NewInt(0),
		})
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	// The flood: numFillers addresses, each with a contiguous chain of
	// zero-fee transactions starting at nonce 0 — immediately executable
	// (pending), not queued. This matters: the pool's pre-existing
	// GlobalQueue limit (1024, far smaller than the reserved cap this task
	// adds) bounds queued transactions pool-wide regardless of reserved
	// status, so a purely-queued (gapped) flood would be truncated by that
	// existing mechanism long before threatening the reserved-occupancy cap
	// at all. A pending flood is instead bounded by GlobalSlots (5120),
	// comfortably above the cap. The fillers' own client quota
	// (reservedPoolOccupancyGas, one transaction's worth) makes mining drain
	// this backlog at most one transaction per block, slow enough that the
	// checks immediately below see it still at the cap. Submitted only to
	// node0: whichever of these the miner does pick per block still needs to
	// reach consensus, but the bulk of the backlog staying resident is a
	// local pool-state concern, so it only needs to exist in one node's pool
	// to prove the local admission gate holds.
	var floodTxs []*types.Transaction
	for _, key := range fillerKeys {
		for n := uint64(0); n < uint64(perFillerChainLen); n++ {
			floodTxs = append(floodTxs, buildReservedTx(key, n))
		}
	}

	var admitted, capRejected int
	for i, err := range nodes[0].TxPool().Add(floodTxs, true) {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, legacypool.ErrReservedOccupancyExceeded):
			capRejected++
		default:
			t.Fatalf("flood tx %d: unexpected error %v", i, err)
		}
	}
	t.Logf("flood: %d/%d admitted, %d rejected for exceeding reserved occupancy (cap %d)", admitted, len(floodTxs), capRejected, wantCap)
	if capRejected == 0 {
		t.Fatalf("expected some flood transactions to be rejected once occupancy hit the cap (%d); got %d admitted, %d rejected", wantCap, admitted, capRejected)
	}
	if admitted > wantCap {
		t.Fatalf("admitted reserved occupancy (%d) exceeds the cap (%d)", admitted, wantCap)
	}

	// A different reserved sender's transaction (whitelisted, but under a
	// separate, otherwise-unused client) must be rejected purely on
	// occupancy grounds: classification isn't the reason, aggregate reserved
	// occupancy is. This is deterministic regardless of block timing —
	// nothing admitted above is ever executable, so occupancy cannot drop on
	// its own while we check.
	probeTx := buildReservedTx(probeKey, 0)
	if err := nodes[0].TxPool().Add([]*types.Transaction{probeTx}, true)[0]; err == nil {
		t.Fatal("probe reserved tx should have been rejected while occupancy is at the cap")
	} else if !errors.Is(err, legacypool.ErrReservedOccupancyExceeded) {
		t.Fatalf("probe reserved tx rejected with unexpected error: %v", err)
	}

	// A normal, fee-paying sender unrelated to either reserved client must
	// still be admitted and mined throughout — headroom genuinely held.
	normalTx, err := types.SignNewTx(normalKey, signer, &types.DynamicFeeTx{
		ChainID:   genesis.Config.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(30_000_000_000),
		GasFeeCap: big.NewInt(100_000_000_000),
		Gas:       reservedPoolOccupancyGas,
		To:        &recipient,
		Value:     big.NewInt(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		for i, err := range node.TxPool().Add([]*types.Transaction{normalTx}, true) {
			if err != nil {
				t.Fatalf("normal sender tx %d rejected: %v", i, err)
			}
		}
	}
	normalBlock := waitMined(normalTx.Hash(), "normal sender's transaction")
	t.Logf("normal sender's transaction mined in block %d while reserved occupancy was saturated", normalBlock.NumberU64())

	// Deregister the flooding client (a realistic governance action: suspend
	// a client entirely). Its transactions stay physically in node0's pool —
	// nothing here evicts them — but they stop counting toward the cap once
	// purged by the next head's Layer-2 recompute, freeing the entire flood's
	// occupancy at once for every other reserved sender, including probeAddr.
	deactivateTx := sendOwnerTx(oNonce+3, 200_000, mustPack(t, setupABI, "setClientActive", big.NewInt(1), false))
	deactivateBlock := waitMined(deactivateTx.Hash(), "setClientActive(1, false)")
	t.Logf("fillers' client deactivated in block %d", deactivateBlock.NumberU64())

	// No permanent starvation: once occupancy has freed up, the
	// previously-rejected probe transaction is admitted. The snapshot
	// rebuild and Layer-2 recompute both run per new head, so poll for a
	// few blocks rather than assuming the very next one already reflects it.
	deadline := time.After(60 * time.Second)
	for {
		err := nodes[0].TxPool().Add([]*types.Transaction{probeTx}, true)[0]
		if err == nil {
			break
		}
		if !errors.Is(err, legacypool.ErrReservedOccupancyExceeded) {
			t.Fatalf("probe reserved tx rejected with unexpected error after deregistration: %v", err)
		}
		select {
		case <-deadline:
			t.Fatal("probe reserved tx was never admitted after the flooding client was deregistered")
		case <-time.After(200 * time.Millisecond):
		}
	}
	// Submit to node1 too, so it's the whole network's canonical chain (not
	// just node0's private pool state) that ends up including it.
	if err := nodes[1].TxPool().Add([]*types.Transaction{probeTx}, true)[0]; err != nil {
		t.Fatalf("probe reserved tx rejected by node1: %v", err)
	}
	probeBlock := waitMined(probeTx.Hash(), "the probe reserved tx")
	t.Logf("probe reserved tx mined in block %d after the flooding client was deregistered", probeBlock.NumberU64())

	// Settle a few confirmations past the last event of interest, then
	// compare canonical chains for produce/verify parity: the second node
	// (which did not exclusively produce this window) must agree.
	settleTarget := probeBlock.NumberU64() + 3
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
}

// mustPack packs an ABI call, failing the test on error. Kept separate from
// the inline setupABI.Pack calls above only because setClientActive's second
// argument is a bare bool literal, which reads awkwardly inline.
func mustPack(t *testing.T, a abi.ABI, method string, args ...interface{}) []byte {
	t.Helper()
	data, err := a.Pack(method, args...)
	if err != nil {
		t.Fatalf("pack %s: %v", method, err)
	}
	return data
}
