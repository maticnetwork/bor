//go:build integration

package bor

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/fdlimit"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
)

// rpcCall is one named RPC invocation of the read battery.
type rpcCall struct {
	name   string
	method string
	params []interface{}
}

// pipelinedRPCTarget bundles what the battery needs to build calls.
type pipelinedRPCTarget struct {
	client    *rpc.Client
	sender    common.Address
	recipient common.Address
}

// TestPipelinedImportSRC_RPCDuringImport verifies that the read-only RPC
// surface stays correct on a node importing with pipelined SRC enabled:
//
//  1. While the importer is syncing, a battery of state-reading RPC methods
//     issued at pinned recent heights and at "latest" must never error —
//     value reads resolve through StateAt's FlatDiff overlay when the queried
//     block is still inside the SRC window, and eth_getProof (which needs
//     committed trie nodes) waits out the window via WaitForStateCommit.
//  2. Values served during the sync (possibly from the overlay) must be
//     identical to values served after everything settles.
//  3. After settle, every height's responses must match a non-pipelined
//     block producer's responses exactly, and account proofs must verify
//     cryptographically against the block state root.
func TestPipelinedImportSRC_RPCDuringImport(t *testing.T) {
	t.Parallel()
	prevLogger := log.Root()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	t.Cleanup(func() { log.SetDefault(prevLogger) })
	_, err := fdlimit.Raise(2048)
	require.NoError(t, err, "raising fd limit")

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], err = crypto.GenerateKey()
		require.NoErrorf(t, err, "generating faucet key %d", i)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0)

	bpStack, bpBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer bpStack.Close()
	bpDataDir := bpStack.DataDir()
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(bpDataDir), "removing block-producer datadir")
	})

	importerStack, importerBackend, err := InitImporterWithPipelinedSRC(genesis, keys[1], true)
	require.NoError(t, err)
	defer importerStack.Close()
	importerDataDir := importerStack.DataDir()
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(importerDataDir), "removing importer datadir")
	})

	// No listener-wait loops needed: connectAndWaitForPeers waits (with a
	// deadline) for both nodes to publish real listener ports before peering.
	connectAndWaitForPeers(t, importerStack, bpStack)
	require.NoError(t, bpBackend.StartMining())

	imp := &pipelinedRPCTarget{
		client:    importerStack.Attach(),
		sender:    crypto.PubkeyToAddress(pkey1.PublicKey),
		recipient: crypto.PubkeyToAddress(pkey2.PublicKey),
	}
	defer imp.client.Close()
	bp := &pipelinedRPCTarget{client: bpStack.Attach(), sender: imp.sender, recipient: imp.recipient}
	defer bp.client.Close()

	// Continuous transfer stream so imported blocks mutate state.
	txStream := newTransferStream(t, bpBackend, pkey1, imp.recipient)

	const targetBlock = uint64(24)
	recorded := make(map[uint64]map[string]json.RawMessage)

	deadline := time.After(240 * time.Second)
	for importerBackend.BlockChain().CurrentBlock().Number.Uint64() < targetBlock {
		select {
		case <-deadline:
			t.Fatalf("timed out syncing to block %d, importer at %d",
				targetBlock, importerBackend.BlockChain().CurrentBlock().Number.Uint64())
		default:
		}
		txStream.sendBatch(2)

		if h := importerBackend.BlockChain().CurrentBlock().Number.Uint64(); h >= 1 {
			if _, seen := recorded[h]; !seen {
				recorded[h] = runStrictBattery(t, imp, pinnedCalls(imp, hexutil.Uint64(h)))
			}
			runStrictBattery(t, imp, pinnedCalls(imp, "latest"))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Let the last block's SRC settle and async writes flush.
	time.Sleep(3 * time.Second)

	verifyRecordedBatteriesStable(t, imp, recorded)

	receipt, err := callRPC(imp.client, "eth_getTransactionReceipt", txStream.firstTxHash)
	require.NoError(t, err)
	require.NotEqual(t, "null", string(receipt), "receipt for first streamed tx must exist on importer")

	compareUpTo := importerBackend.BlockChain().CurrentBlock().Number.Uint64()
	if bpNum := bpBackend.BlockChain().CurrentBlock().Number.Uint64(); bpNum < compareUpTo {
		compareUpTo = bpNum
	}
	assertRPCParity(t, imp, bp, compareUpTo)
	t.Logf("RPC battery: %d pinned heights recorded, parity verified up to block %d",
		len(recorded), compareUpTo)
}

// pinnedCalls builds the strict battery: state-reading RPC methods that must
// never fail at the given height, SRC window or not.
func pinnedCalls(target *pipelinedRPCTarget, blockArg interface{}) []rpcCall {
	callArgs := map[string]interface{}{
		"from":  target.sender,
		"to":    target.recipient,
		"value": "0x1",
	}
	return []rpcCall{
		{"getBalance-sender", "eth_getBalance", []interface{}{target.sender, blockArg}},
		{"getBalance-recipient", "eth_getBalance", []interface{}{target.recipient, blockArg}},
		{"getTransactionCount", "eth_getTransactionCount", []interface{}{target.sender, blockArg}},
		{"getCode", "eth_getCode", []interface{}{target.recipient, blockArg}},
		{"getStorageAt", "eth_getStorageAt", []interface{}{target.recipient, "0x0", blockArg}},
		{"call", "eth_call", []interface{}{callArgs, blockArg}},
		{"estimateGas", "eth_estimateGas", []interface{}{callArgs, blockArg}},
		{"getBlockByNumber", "eth_getBlockByNumber", []interface{}{blockArg, true}},
		{"getBlockReceipts", "eth_getBlockReceipts", []interface{}{blockArg}},
		{"getLogs", "eth_getLogs", []interface{}{map[string]interface{}{"fromBlock": blockArg, "toBlock": blockArg}}},
		{"traceBlockByNumber", "debug_traceBlockByNumber", []interface{}{blockArg, nil}},
		{"getProof", "eth_getProof", []interface{}{target.sender, []string{}, blockArg}},
	}
}

func runStrictBattery(t *testing.T, target *pipelinedRPCTarget, calls []rpcCall) map[string]json.RawMessage {
	t.Helper()
	results := make(map[string]json.RawMessage, len(calls))
	for _, c := range calls {
		res, err := callRPC(target.client, c.method, c.params...)
		require.NoError(t, err, "%s (%s) must not fail", c.name, c.method)
		results[c.name] = res
	}
	return results
}

// verifyRecordedBatteriesStable re-runs every pinned battery after full settle
// and requires responses identical to what was served during the sync — a
// value served from the FlatDiff overlay inside the SRC window must match the
// committed trie exactly.
func verifyRecordedBatteriesStable(t *testing.T, target *pipelinedRPCTarget, recorded map[uint64]map[string]json.RawMessage) {
	t.Helper()
	for h, want := range recorded {
		got := runStrictBattery(t, target, pinnedCalls(target, hexutil.Uint64(h)))
		for name, wantRes := range want {
			require.JSONEq(t, string(wantRes), string(got[name]),
				"%s at height %d changed between sync-time and settled read", name, h)
		}
	}
}

// assertRPCParity compares the settled importer's responses against the
// non-pipelined BP for every height, and cryptographically verifies the
// importer's account proofs against each block's state root.
func assertRPCParity(t *testing.T, imp, bp *pipelinedRPCTarget, upTo uint64) {
	t.Helper()
	parityMethods := []rpcCall{
		{"getBalance-sender", "eth_getBalance", nil},
		{"getBalance-recipient", "eth_getBalance", nil},
		{"getTransactionCount", "eth_getTransactionCount", nil},
	}
	for h := uint64(1); h <= upTo; h++ {
		blockArg := hexutil.Uint64(h)
		for _, m := range parityMethods {
			addr := imp.sender
			if m.name == "getBalance-recipient" {
				addr = imp.recipient
			}
			impRes, err := callRPC(imp.client, m.method, addr, blockArg)
			require.NoError(t, err)
			bpRes, err := callRPC(bp.client, m.method, addr, blockArg)
			require.NoError(t, err)
			require.JSONEq(t, string(bpRes), string(impRes), "%s parity at height %d", m.name, h)
		}
		impBlock, err := callRPC(imp.client, "eth_getBlockByNumber", blockArg, true)
		require.NoError(t, err)
		bpBlock, err := callRPC(bp.client, "eth_getBlockByNumber", blockArg, true)
		require.NoError(t, err)
		require.JSONEq(t, string(bpBlock), string(impBlock), "block %d JSON parity", h)

		impReceipts, err := callRPC(imp.client, "eth_getBlockReceipts", blockArg)
		require.NoError(t, err)
		bpReceipts, err := callRPC(bp.client, "eth_getBlockReceipts", blockArg)
		require.NoError(t, err)
		require.JSONEq(t, string(bpReceipts), string(impReceipts), "block %d receipts parity", h)

		verifyAccountProofParity(t, imp, bp, blockArg, impBlock)
	}
}

// verifyAccountProofParity checks getProof equality between the two nodes and
// verifies the importer's account proof against the block's state root.
func verifyAccountProofParity(t *testing.T, imp, bp *pipelinedRPCTarget, blockArg hexutil.Uint64, blockJSON json.RawMessage) {
	t.Helper()
	impProof, err := callRPC(imp.client, "eth_getProof", imp.sender, []string{}, blockArg)
	require.NoError(t, err, "eth_getProof on importer at height %d after settle", blockArg)
	bpProof, err := callRPC(bp.client, "eth_getProof", bp.sender, []string{}, blockArg)
	require.NoError(t, err)
	require.JSONEq(t, string(bpProof), string(impProof), "getProof parity at height %d", blockArg)

	var header struct {
		StateRoot common.Hash `json:"stateRoot"`
	}
	require.NoError(t, json.Unmarshal(blockJSON, &header))
	var proof struct {
		AccountProof []hexutil.Bytes `json:"accountProof"`
	}
	require.NoError(t, json.Unmarshal(impProof, &proof))

	proofDb := memorydb.New()
	for _, node := range proof.AccountProof {
		require.NoError(t, proofDb.Put(crypto.Keccak256(node), node))
	}
	val, err := trie.VerifyProof(header.StateRoot, crypto.Keccak256(imp.sender.Bytes()), proofDb)
	require.NoError(t, err, "account proof at height %d must verify against state root %x", blockArg, header.StateRoot)
	require.NotEmpty(t, val, "account proof at height %d must resolve to a non-empty account", blockArg)
}

func callRPC(client *rpc.Client, method string, params ...interface{}) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var res json.RawMessage
	err := client.CallContext(ctx, &res, method, params...)
	return res, err
}

// transferStream submits plain transfers to the BP's txpool so imported
// blocks carry state mutations.
type transferStream struct {
	t           *testing.T
	pool        *txpool.TxPool
	signer      types.Signer
	key         *ecdsa.PrivateKey
	to          common.Address
	nonce       uint64
	sent        int
	firstTxHash common.Hash
}

const transferStreamMaxTxs = 40

func newTransferStream(t *testing.T, bpBackend *eth.Ethereum, key *ecdsa.PrivateKey, to common.Address) *transferStream {
	sender := crypto.PubkeyToAddress(key.PublicKey)
	pool := bpBackend.TxPool()
	return &transferStream{
		t:      t,
		pool:   pool,
		signer: types.LatestSignerForChainID(bpBackend.BlockChain().Config().ChainID),
		key:    key,
		to:     to,
		nonce:  pool.Nonce(sender),
	}
}

func (s *transferStream) sendBatch(n int) {
	s.t.Helper()
	for i := 0; i < n && s.sent < transferStreamMaxTxs; i++ {
		tx := types.NewTransaction(s.nonce, s.to, big.NewInt(1000), 21000, big.NewInt(30000000000), nil)
		signedTx, err := types.SignTx(tx, s.signer, s.key)
		require.NoError(s.t, err)
		errs := s.pool.Add([]*types.Transaction{signedTx}, true)
		require.NoError(s.t, errs[0], "failed to add streamed tx %d", s.sent)
		if s.sent == 0 {
			s.firstTxHash = signedTx.Hash()
		}
		s.nonce++
		s.sent++
	}
}
