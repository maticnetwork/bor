package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/triedb"
)

func delegatedSenderKey(tag byte) *ecdsa.PrivateKey {
	seed := sha256.Sum256([]byte{tag, 0x01})
	k, err := crypto.ToECDSA(seed[:])
	if err != nil {
		panic(err)
	}
	return k
}

// TestV2_DelegatedSenderNonceParity guards the parallel/serial parity property
// for a sender that carries an EIP-7702 delegation designator installed in an
// EARLIER block. Such a sender appears in no authorization list of the current
// block, so computeSenderNonces' auth-list scan misses it. A CALL into the
// delegated account runs the delegate code in the account's own frame; a CREATE
// there bumps the sender's nonce mid-block. Pre-computing SenderNonces for that
// sender would hand a later same-sender tx a stale nonce that BlockSTM V2 would
// accept while the serial processor rejects it with "nonce too low" — a
// per-node race outcome and thus a consensus split.
//
// With the base-state-code exclusion, V2 routes the sender through the
// MVStore-aware nonce path and rejects the block exactly as serial does.
func TestV2_DelegatedSenderNonceParity(t *testing.T) {
	keyA := delegatedSenderKey(1) // delegated EOA, sends two txs in the block
	keyB := delegatedSenderKey(2) // pokes A's delegate code via a plain CALL
	addrA := crypto.PubkeyToAddress(keyA.PublicKey)
	addrB := crypto.PubkeyToAddress(keyB.PublicKey)
	// delegate target runtime: PUSH1 0; PUSH1 0; PUSH1 0; CREATE; POP; STOP
	target := common.HexToAddress("0x00000000000000000000000000000000000d1e6a")
	createCode := common.FromHex("600060006000f05000")
	sink := common.HexToAddress("0x00000000000000000000000000000000000051f5")

	cfg := fuzzChainConfig
	bal, _ := new(big.Int).SetString("1000000000000000000000", 10)
	// A carries the delegation designator at base state (installed earlier).
	deleg := append([]byte{0xef, 0x01, 0x00}, target.Bytes()...)

	gspec := &Genesis{
		Config: cfg,
		Alloc: types.GenesisAlloc{
			addrA:  {Balance: bal, Code: deleg},
			addrB:  {Balance: bal},
			target: {Balance: common.Big0, Code: createCode},
		},
		GasLimit: 30_000_000,
	}

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	genesis := gspec.MustCommit(memdb, tdb)

	signer := types.LatestSigner(cfg)
	mk := func(key *ecdsa.PrivateKey, nonce uint64, to common.Address, gas uint64) *types.Transaction {
		return types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID: cfg.ChainID, Nonce: nonce, To: &to, Gas: gas,
			GasFeeCap: big.NewInt(1_000_000_000), GasTipCap: big.NewInt(1),
			Value: big.NewInt(0),
		})
	}
	txs := types.Transactions{
		mk(keyA, 0, sink, 60_000),   // A@0
		mk(keyB, 0, addrA, 200_000), // B -> A: delegate code runs CREATE, bumps A's nonce to 2
		mk(keyA, 1, sink, 60_000),   // A@1: serial-invalid once A's nonce is 2
	}

	base, err := state.New(genesis.Root(), state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatalf("base state: %v", err)
	}
	finalDB, err := state.New(genesis.Root(), state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatalf("final state: %v", err)
	}
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer, Transfer: Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		GasLimit:    gspec.GasLimit,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(0),
		Random:      &common.Hash{},
	}
	tasks := make([]V2Task, len(txs))
	for i, tx := range txs {
		m, err := TransactionToMessage(tx, signer, blockCtx.BaseFee)
		if err != nil {
			t.Fatalf("tx %d to message: %v", i, err)
		}
		tasks[i] = V2Task{Index: i, Tx: tx, Msg: m}
	}

	res := ExecuteV2BlockSTM(context.Background(), tasks, base, blockstm.NewMVStore(),
		blockstm.NewMVBalanceStore(), blockCtx, common.Hash{}, vm.Config{}, cfg, gspec.GasLimit, 4, finalDB, nil)

	// tx2 declares nonce 1, but A's real nonce is 2 after tx1's CREATE. V2 must
	// reject it — matching the serial processor — rather than accept a stale
	// pre-computed nonce.
	if res.ExecErrIdx != 2 {
		t.Fatalf("V2 accepted the delegated-sender block (ExecErrIdx=%d, err=%v); "+
			"expected rejection at tx2 with a nonce error, matching the serial processor. "+
			"computeSenderNonces handed out a stale pre-computed nonce for a base-state-delegated sender.",
			res.ExecErrIdx, res.ExecErr)
	}
}
