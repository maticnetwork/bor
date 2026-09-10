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
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// TestReservedProduceImportFieldParity is the produce-vs-import counterpart to
// the sibling reserved-blockspace integration tests (reserved_blockspace_test.go,
// reserved_capacity_test.go, reserved_receipts_test.go), which all assert
// parity only by convergence: node0 (the producer) and node1 (the
// verifier/importer) end up agreeing on the same canonical chain. Convergence
// is a real consensus-parity signal - a genuine classification split would
// make node1 reject node0's block outright, never converge - but it doesn't
// pin down which persisted fields were recomputed identically, only that the
// two sides landed on the same hash by whatever means. This test instead
// recomputes and diffs, field by field, on both nodes: the header's stamped
// ReservedGasUsed/ReservedCapacity, the on-disk reserved-tx index side table
// (rawdb.ReadReservedTxIndexes), and each transaction's derived effective gas
// price - turning "they converged" into an explicit produce-vs-import field
// diff.
//
// Three registry clients exercise the three classification outcomes in one
// run: a sole-member zero-fee client (always reserved), a sole-member
// fallback-fee client whose quota exactly covers its one transaction
// (reserved despite carrying a real fee, per POS-3671), and a two-member
// client whose quota fits only one transaction (mirroring
// TestReservedBlockspaceQuotaOverflowPaysNormalFees's arithmetic): the
// zero-fee member wins the shared quota and the fallback-fee member overflows
// into the normal, fee-paying region.
//
// Run with: go test -tags=integration -run TestReservedProduceImportFieldParity ./tests/bor/
func TestReservedProduceImportFieldParity(t *testing.T) {
	faucets := make([]*ecdsa.PrivateKey, 10)
	for i := range faucets {
		faucets[i], _ = crypto.GenerateKey()
	}
	zeroKey := faucets[0]           // client A (sole member): reserved, zero-fee, in quota
	fallbackKey := faucets[1]       // client B (sole member): reserved, fallback-fee, in quota
	overflowWinnerKey := faucets[2] // client C member 1: zero-fee, wins the shared quota
	overflowKey := faucets[3]       // client C member 2: fallback-fee, overflows to normal fees
	ownerKey := faucets[4]

	zeroAddr := crypto.PubkeyToAddress(zeroKey.PublicKey)
	fallbackAddr := crypto.PubkeyToAddress(fallbackKey.PublicKey)
	overflowWinnerAddr := crypto.PubkeyToAddress(overflowWinnerKey.PublicKey)
	overflowAddr := crypto.PubkeyToAddress(overflowKey.PublicKey)
	ownerAddr := crypto.PubkeyToAddress(ownerKey.PublicKey)

	registryAddr := common.HexToAddress(params.DefaultReservedRegistryContract)
	setupABI, err := abi.JSON(strings.NewReader(reservedRegistrySetupABI))
	if err != nil {
		t.Fatal(err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
	const reservedFork = uint64(5)
	genesis.Config.Bor.ReservedBlockspaceBlock = new(big.Int).SetUint64(reservedFork)
	// Giugliano must not activate after reserved blockspace (its base-fee params
	// are earlier optional BlockExtraData fields than ReservedGasUsed); co-activate
	// them so post-fork blocks stamp all the optional fields together.
	genesis.Config.Bor.GiuglianoBlock = new(big.Int).SetUint64(reservedFork)
	// The registry runtime bytecode uses PUSH0, a Shanghai opcode; activate
	// Shanghai before the fork so the contract is callable in time to seed it.
	genesis.Config.ShanghaiBlock = big.NewInt(2)
	genesis.Config.Bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	genesis.Alloc[registryAddr] = types.Account{
		Balance: new(big.Int),
		Code:    common.FromHex(params.ReservedBlockspaceRegistryCode),
	}

	startBalance := new(big.Int).SetUint64(1_000_000_000_000_000_000) // 1 ETH
	for _, a := range []common.Address{zeroAddr, fallbackAddr, overflowWinnerAddr, overflowAddr, ownerAddr} {
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

	// London activates at block 1 in this genesis; the dynamic-fee setup txs
	// are rejected before then ("pool not yet in London").
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
	const clientQuota = uint64(21_000) // exactly one simple transfer per client
	createA := packCreate(clientQuota, "A-zero", []common.Address{zeroAddr})
	createB := packCreate(clientQuota, "B-fallback", []common.Address{fallbackAddr})
	// Client C: quota fits exactly one of its two members' transactions, so
	// the second unavoidably overflows to the normal fee-paying region.
	createC := packCreate(clientQuota, "C-overflow", []common.Address{overflowWinnerAddr, overflowAddr})

	oNonce, err := nodes[0].APIBackend.GetPoolNonce(context.Background(), ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendOwnerTx(oNonce, initData)
	sendOwnerTx(oNonce+1, createA)
	sendOwnerTx(oNonce+2, createB)
	lastCreate := sendOwnerTx(oNonce+3, createC)
	seedBlock := waitForBorTxMined(t, nodes, lastCreate.Hash(), "createClient C")

	// Land the test transactions inside node0's primary-producer window (see
	// the sibling files' identical reasoning: sprint=8 with two validators
	// gives node0 blocks 1-8 and 17-24, node1 blocks 9-16), so the chain
	// doesn't reorg them out at a producer handover. The registry must be
	// fully seeded well before that window opens.
	if seedBlock.NumberU64() >= 16 {
		t.Fatalf("registry seeded too late (block %d) for the 17-24 producer window", seedBlock.NumberU64())
	}
	waitForBorBlockHeight(t, nodes, 17, 120*time.Second)

	recipient := common.HexToAddress("0x00000000000000000000000000000000000000aa")

	zeroTx, err := types.SignNewTx(zeroKey, signer, &types.DynamicFeeTx{
		ChainID: genesis.Config.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(0),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	fallbackTx, err := types.SignNewTx(fallbackKey, signer, &types.DynamicFeeTx{
		ChainID: genesis.Config.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(2_000_000_000),
		Gas: 21000, To: &recipient, Value: big.NewInt(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Client C's two members: overflowWinnerTx is zero-fee (always wins the
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

	// inQuotaTxs pay effectiveGasPrice 0 (reserved, admitted within their
	// client's quota); overflowTx is the one exception that must pay real
	// fees despite belonging to a registered client.
	inQuotaTxs := []*types.Transaction{zeroTx, fallbackTx, overflowWinnerTx}
	allTxs := []*types.Transaction{zeroTx, fallbackTx, overflowWinnerTx, overflowTx}
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
		if bn := waitForBorTxMined(t, nodes, tx.Hash(), "produce-import parity tx").NumberU64(); bn > maxBlk {
			maxBlk = bn
		}
	}

	// Settle: wait for both nodes to converge on the tx blocks with a few
	// confirmations, so fork choice has resolved before recomputing fields.
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
			t.Fatal("nodes never converged on the produce-import parity tx blocks")
		case <-time.After(200 * time.Millisecond):
		}
	}

	// The four transactions may share one canonical block or spread across a
	// couple depending on pool/miner timing; collect the distinct inclusion
	// blocks and diff every field this test cares about on both nodes for
	// each of them.
	inclusionBlocks := map[uint64]common.Hash{}
	reservedGasByBlock := map[uint64]uint64{}
	for _, tx := range allTxs {
		blk := findBlockContaining(t, nodes[0], tx.Hash())
		inclusionBlocks[blk.NumberU64()] = blk.Hash()
	}
	for _, tx := range inQuotaTxs {
		blk := findBlockContaining(t, nodes[0], tx.Hash())
		reservedGasByBlock[blk.NumberU64()] += 21000
	}

	for number, hash := range inclusionBlocks {
		b0 := nodes[0].BlockChain().GetBlockByNumber(number)
		b1 := nodes[1].BlockChain().GetBlockByNumber(number)
		if b0 == nil || b1 == nil {
			t.Fatalf("block %d missing on a node (node0=%v node1=%v)", number, b0 != nil, b1 != nil)
		}
		if b0.Hash() != hash || b1.Hash() != hash {
			t.Fatalf("block %d hash mismatch: node0=%s node1=%s want=%s", number, b0.Hash(), b1.Hash(), hash)
		}

		// (1) Header field parity: the stamped reserved gas/capacity must be
		// identical between producer and importer, and must count only the
		// in-quota gas actually landing in this block - the overflow tx's gas
		// is never reserved gas, wherever it lands.
		gasUsed0, capacity0 := b0.Header().GetReservedFields(genesis.Config)
		gasUsed1, capacity1 := b1.Header().GetReservedFields(genesis.Config)
		if gasUsed0 == nil || gasUsed1 == nil {
			t.Fatalf("block %d missing ReservedGasUsed header field (node0=%v node1=%v)", number, gasUsed0, gasUsed1)
		}
		if *gasUsed0 != *gasUsed1 {
			t.Fatalf("block %d ReservedGasUsed producer=%d importer=%d", number, *gasUsed0, *gasUsed1)
		}
		wantGas := reservedGasByBlock[number]
		if *gasUsed0 != wantGas {
			t.Fatalf("block %d ReservedGasUsed = %d, want %d (in-quota gas only, overflow excluded)", number, *gasUsed0, wantGas)
		}
		if capacity0 == nil || capacity1 == nil {
			t.Fatalf("block %d missing ReservedCapacity header field (node0=%v node1=%v)", number, capacity0, capacity1)
		}
		if *capacity0 != *capacity1 {
			t.Fatalf("block %d ReservedCapacity producer=%d importer=%d", number, *capacity0, *capacity1)
		}
		const wantCapacity = 3 * clientQuota // clients A, B, C, all immediately effective
		if *capacity0 != wantCapacity {
			t.Fatalf("block %d ReservedCapacity = %d, want %d", number, *capacity0, wantCapacity)
		}

		// (2) On-disk reserved-tx index side table parity: the persisted
		// classification a fresh read (RPC, re-derivation) relies on must
		// agree byte-for-byte between the block that produced it and the
		// block that only verified it.
		idx0 := rawdb.ReadReservedTxIndexes(nodes[0].ChainDb(), hash, number)
		idx1 := rawdb.ReadReservedTxIndexes(nodes[1].ChainDb(), hash, number)
		if len(idx0) != len(idx1) {
			t.Fatalf("block %d reserved-tx indexes length: producer=%v importer=%v", number, idx0, idx1)
		}
		for i := range idx0 {
			if idx0[i] != idx1[i] {
				t.Fatalf("block %d reserved-tx indexes: producer=%v importer=%v", number, idx0, idx1)
			}
		}
	}

	// (3) Per-transaction receipt effective gas price parity: exactly zero
	// for every in-quota transaction, non-zero for the overflowed one,
	// identical on both nodes.
	inQuotaSet := make(map[common.Hash]bool, len(inQuotaTxs))
	for _, tx := range inQuotaTxs {
		inQuotaSet[tx.Hash()] = true
	}
	for _, tx := range allTxs {
		r0 := findReceipt(t, nodes[0], tx.Hash())
		r1 := findReceipt(t, nodes[1], tx.Hash())
		if r0.EffectiveGasPrice.Cmp(r1.EffectiveGasPrice) != 0 {
			t.Fatalf("tx %s effectiveGasPrice: producer=%s importer=%s", tx.Hash(), r0.EffectiveGasPrice, r1.EffectiveGasPrice)
		}
		wantZero := inQuotaSet[tx.Hash()]
		gotZero := r0.EffectiveGasPrice.Sign() == 0
		if gotZero != wantZero {
			t.Fatalf("tx %s effectiveGasPrice = %s, want zero=%v", tx.Hash(), r0.EffectiveGasPrice, wantZero)
		}
	}
}
