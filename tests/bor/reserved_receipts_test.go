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
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// TestReservedBlockspaceEffectiveGasPriceRPC is the POS-3670 end-to-end
// validation: every reserved-region receipt and transaction view reports
// effectiveGasPrice/gasPrice 0, including for a reserved sender that carries a
// real (fallback) fee but executes fee-free within its quota - the case the
// fee-derived formula gets wrong without the persisted classification this
// task adds.
//
// Four independent single-member clients avoid any cross-sender quota
// ordering ambiguity for the three "reserved" cases: a zero-fee sender, a
// fallback-fee dynamic-fee sender, and a fallback-fee legacy sender, each the
// sole member of a client whose quota fits exactly its one transaction, so
// each is unambiguously reserved. A fifth client has two members sharing a
// quota that fits only one transaction - a zero-fee sender (which always
// wins the ascending-price reserved selection) and a fallback-fee sender -
// exercising a genuine quota overflow into the normal, fee-paying region
// (mirroring TestReservedBlockspaceQuotaOverflowPaysNormalFees's proven
// pattern) for the fourth case.
//
// Assertions go through eth_getTransactionReceipt, eth_getBlockReceipts, and
// the three eth_getTransactionBy* views (via the same ethapi handlers the
// JSON-RPC server dispatches to), on both the producing node and a peer that
// only verified the block, so the check also pins produce/verify parity for
// the persisted side table.
//
// Run with: go test -tags=integration -run TestReservedBlockspaceEffectiveGasPriceRPC ./tests/bor/
func TestReservedBlockspaceEffectiveGasPriceRPC(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 10)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	zeroKey := faucets[0]           // client A (sole member): reserved, zero-fee
	dynKey := faucets[1]            // client B (sole member): reserved, fallback-fee (dynamic-fee type)
	legacyKey := faucets[2]         // client C (sole member): reserved, fallback-fee (legacy type)
	overflowWinnerKey := faucets[3] // client D member 1: zero-fee, wins the shared quota
	overflowKey := faucets[4]       // client D member 2: fallback-fee, overflows to normal
	ownerKey := faucets[5]

	zeroAddr := crypto.PubkeyToAddress(zeroKey.PublicKey)
	dynAddr := crypto.PubkeyToAddress(dynKey.PublicKey)
	legacyAddr := crypto.PubkeyToAddress(legacyKey.PublicKey)
	overflowWinnerAddr := crypto.PubkeyToAddress(overflowWinnerKey.PublicKey)
	overflowAddr := crypto.PubkeyToAddress(overflowKey.PublicKey)
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)

	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	setupABI, err := abi.JSON(strings.NewReader(reservedRegistrySetupABI))
	if err != nil {
		t.Fatal(err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	reservedFork := uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}

	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000) // 1 ETH
	for _, a := range []common.Address{zeroAddr, dynAddr, legacyAddr, overflowWinnerAddr, overflowAddr, ownerAddr} {
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
				t.Fatalf("timeout waiting for block %d (at %d)", target, nodes[0].BlockChain().CurrentBlock().Number.Uint64())
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	waitMined := func(txHash common.Hash, what string) *types.Block {
		t.Helper()
		deadline := time.After(60 * time.Second)
		for {
			if blk := blockContaining(nodes[0], txHash); blk != nil {
				return blk
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
	packCreate := func(quota uint64, metadata string, addrs []common.Address) []byte {
		t.Helper()
		data, err := setupABI.Pack("createClient", ownerAddr, quota, uint8(0), uint64(0), metadata, addrs)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	createA := packCreate(21_000, "A-zero", []common.Address{zeroAddr})
	createB := packCreate(21_000, "B-dyn-fallback", []common.Address{dynAddr})
	createC := packCreate(21_000, "C-legacy-fallback", []common.Address{legacyAddr})
	// Client D: quota fits exactly one of its two members' transactions.
	createD := packCreate(21_000, "D-overflow", []common.Address{overflowWinnerAddr, overflowAddr})

	waitForBlock(2)
	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	sendOwnerTx(oNonce+1, createA)
	sendOwnerTx(oNonce+2, createB)
	sendOwnerTx(oNonce+3, createC)
	lastCreate := sendOwnerTx(oNonce+4, createD)
	seedBlock := waitMined(lastCreate.Hash(), "createClient D")
	if seedBlock.NumberU64() >= 16 {
		t.Fatalf("registry seeded too late (block %d) for the 17-24 producer window", seedBlock.NumberU64())
	}
	waitForBlock(17)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	zeroTx, err := types.SignNewTx(zeroKey, signer, &types.DynamicFeeTx{
		ChainID: genesis.Config.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(0),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	dynTx, err := types.SignNewTx(dynKey, signer, &types.DynamicFeeTx{
		ChainID: genesis.Config.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(2_000_000_000),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyTx, err := types.SignNewTx(legacyKey, signer, &types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(2_000_000_000),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Client D's two members: overflowWinnerTx is zero-fee (always wins the
	// ascending-price reserved selection), overflowTx carries a real fee and
	// overflows the one-transaction quota to the normal, fee-paying region.
	overflowWinnerTx, err := types.SignNewTx(overflowWinnerKey, signer, &types.DynamicFeeTx{
		ChainID: genesis.Config.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(0),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	overflowTx, err := types.SignNewTx(overflowKey, signer, &types.DynamicFeeTx{
		ChainID: genesis.Config.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(5_000_000_000), GasFeeCap: big.NewInt(10_000_000_000),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}

	allTxs := []*types.Transaction{zeroTx, dynTx, legacyTx, overflowWinnerTx, overflowTx}
	for _, node := range nodes {
		for _, tx := range allTxs {
			if err := node.APIBackend.SendTx(context.Background(), tx); err != nil &&
				!strings.Contains(err.Error(), "already known") {
				t.Fatalf("tx %s rejected by pool: %v", tx.Hash(), err)
			}
		}
	}

	var maxBlk uint64
	for _, tx := range allTxs {
		if bn := waitMined(tx.Hash(), "reserved-receipts tx").NumberU64(); bn > maxBlk {
			maxBlk = bn
		}
	}

	// Settle: wait for both nodes to converge on the tx blocks with a few
	// confirmations, so fork choice has resolved.
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

	// want maps each tx to its expected effectiveGasPrice/gasPrice: 0 for the
	// three reserved ones, the real market price for the overflow one.
	want := map[common.Hash]*big.Int{
		zeroTx.Hash():           big.NewInt(0),
		dynTx.Hash():            big.NewInt(0),
		legacyTx.Hash():         big.NewInt(0),
		overflowWinnerTx.Hash(): big.NewInt(0),
		overflowTx.Hash():       nil, // filled in from the overflow receipt itself below
	}

	assertNode := func(t *testing.T, n *eth.Ethereum, label string) {
		t.Helper()

		nonceLock := new(ethapi.AddrLocker)
		txAPI := ethapi.NewTransactionAPI(n.APIBackend, nonceLock)
		chainAPI := ethapi.NewBlockChainAPI(n.APIBackend)
		ctx := context.Background()

		// Fill in the overflow tx's expected price from its own receipt: it
		// must be non-zero (it paid real fees), and every other view must
		// agree with whatever that receipt says.
		overflowReceipt, err := txAPI.GetTransactionReceipt(ctx, overflowTx.Hash())
		if err != nil || overflowReceipt == nil {
			t.Fatalf("[%s] overflow tx receipt not found: %v", label, err)
		}
		gotOverflowPrice := effectiveGasPriceOf(t, overflowReceipt)
		if gotOverflowPrice.Sign() == 0 {
			t.Fatalf("[%s] overflow tx effectiveGasPrice = 0, want non-zero (quota overflow must pay normal fees)", label)
		}
		want[overflowTx.Hash()] = gotOverflowPrice

		for _, tx := range allTxs {
			wantPrice := want[tx.Hash()]

			// eth_getTransactionReceipt
			receipt, err := txAPI.GetTransactionReceipt(ctx, tx.Hash())
			if err != nil || receipt == nil {
				t.Fatalf("[%s] GetTransactionReceipt(%s) failed: %v", label, tx.Hash(), err)
			}
			if got := effectiveGasPriceOf(t, receipt); got.Cmp(wantPrice) != 0 {
				t.Errorf("[%s] GetTransactionReceipt(%s).effectiveGasPrice = %s, want %s", label, tx.Hash(), got, wantPrice)
			}

			// eth_getTransactionByHash
			byHash, err := txAPI.GetTransactionByHash(ctx, tx.Hash())
			if err != nil || byHash == nil {
				t.Fatalf("[%s] GetTransactionByHash(%s) failed: %v", label, tx.Hash(), err)
			}
			if got := (*big.Int)(byHash.GasPrice); got.Cmp(wantPrice) != 0 {
				t.Errorf("[%s] GetTransactionByHash(%s).gasPrice = %s, want %s", label, tx.Hash(), got, wantPrice)
			}

			blk := findBlockContaining(t, n, tx.Hash())
			idx := indexInBlock(t, blk, tx.Hash())

			// eth_getTransactionByBlockHashAndIndex
			byBlockHash, err := txAPI.GetTransactionByBlockHashAndIndex(ctx, blk.Hash(), hexutil.Uint(idx))
			if err != nil || byBlockHash == nil {
				t.Fatalf("[%s] GetTransactionByBlockHashAndIndex(%s, %d) failed: %v", label, blk.Hash(), idx, err)
			}
			if got := (*big.Int)(byBlockHash.GasPrice); got.Cmp(wantPrice) != 0 {
				t.Errorf("[%s] GetTransactionByBlockHashAndIndex(%s, %d).gasPrice = %s, want %s", label, blk.Hash(), idx, got, wantPrice)
			}

			// eth_getTransactionByBlockNumberAndIndex
			byBlockNumber, err := txAPI.GetTransactionByBlockNumberAndIndex(ctx, rpc.BlockNumber(blk.NumberU64()), hexutil.Uint(idx))
			if err != nil || byBlockNumber == nil {
				t.Fatalf("[%s] GetTransactionByBlockNumberAndIndex(%d, %d) failed: %v", label, blk.NumberU64(), idx, err)
			}
			if got := (*big.Int)(byBlockNumber.GasPrice); got.Cmp(wantPrice) != 0 {
				t.Errorf("[%s] GetTransactionByBlockNumberAndIndex(%d, %d).gasPrice = %s, want %s", label, blk.NumberU64(), idx, got, wantPrice)
			}

			// eth_getBlockReceipts
			blockReceipts, err := chainAPI.GetBlockReceipts(ctx, rpc.BlockNumberOrHashWithHash(blk.Hash(), false))
			if err != nil {
				t.Fatalf("[%s] GetBlockReceipts(%s) failed: %v", label, blk.Hash(), err)
			}
			found := false
			for _, r := range blockReceipts {
				if r["transactionHash"].(common.Hash) != tx.Hash() {
					continue
				}
				found = true
				if got := effectiveGasPriceOf(t, r); got.Cmp(wantPrice) != 0 {
					t.Errorf("[%s] GetBlockReceipts(%s)[%s].effectiveGasPrice = %s, want %s", label, blk.Hash(), tx.Hash(), got, wantPrice)
				}
			}
			if !found {
				t.Fatalf("[%s] GetBlockReceipts(%s) did not contain tx %s", label, blk.Hash(), tx.Hash())
			}
		}
	}

	// Assert on the producing node and on a peer that only imported and
	// verified the block: the persisted side table must agree everywhere.
	assertNode(t, nodes[0], "producer")
	assertNode(t, nodes[1], "verifier")
}

// blockContaining returns the canonical block holding txHash on node n, or
// nil if none does (yet). The single scan shared by waitMined (which retries
// it until a deadline) and findBlockContaining (which expects it to already
// be there, past the settle point, and fails hard on a miss).
func blockContaining(n *eth.Ethereum, txHash common.Hash) *types.Block {
	head := n.BlockChain().CurrentBlock().Number.Uint64()
	for h := uint64(0); h <= head; h++ {
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

// findBlockContaining is blockContaining with a hard failure on a miss.
func findBlockContaining(t *testing.T, n *eth.Ethereum, txHash common.Hash) *types.Block {
	t.Helper()
	blk := blockContaining(n, txHash)
	if blk == nil {
		t.Fatalf("block containing tx %s not found", txHash)
	}
	return blk
}

// indexInBlock returns txHash's position within block.Transactions().
func indexInBlock(t *testing.T, block *types.Block, txHash common.Hash) int {
	t.Helper()
	for i, tx := range block.Transactions() {
		if tx.Hash() == txHash {
			return i
		}
	}
	t.Fatalf("tx %s not found in block %s", txHash, block.Hash())
	return -1
}

// effectiveGasPriceOf extracts effectiveGasPrice from a marshalled receipt map
// (as returned by ethapi's GetTransactionReceipt/GetBlockReceipts) as *big.Int.
func effectiveGasPriceOf(t *testing.T, receipt map[string]interface{}) *big.Int {
	t.Helper()
	v, ok := receipt["effectiveGasPrice"].(*hexutil.Big)
	if !ok || v == nil {
		t.Fatalf("receipt missing effectiveGasPrice: %+v", receipt)
	}
	return (*big.Int)(v)
}
