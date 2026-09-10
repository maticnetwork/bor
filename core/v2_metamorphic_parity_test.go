package core

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// A metamorphic contract — created, destroyed, and conditionally re-created at
// the same CREATE2 address within one block — must produce the same state root
// under V2 BlockSTM as under serial execution. The conditional re-create makes a
// later EXTCODEHASH read's winning CodePath writer depend on a value an earlier
// (deliberately heavy) tx writes, so V2 speculates the re-create, then withdraws
// it on re-execution; validation must not accept the now-stale code read by value
// equality.
//
// Unlike the fuzzer's light-tx metamorphic seeds (which reproduce this only when
// the scheduler happens to interleave the speculative deploy ahead of the heavy
// tx — empirically a few percent of runs), the heavy gas-burn flag tx here forces
// that interleaving, so this is a reliable, deterministic CI regression guard.
// The loop hedges against the residual scheduling nondeterminism of BlockSTM.
func TestV2SerialParity_MetamorphicCreate2(t *testing.T) {
	// f: factory + victim.
	//   empty calldata : CREATE2-deploy tgt then CALL it so tgt SELFDESTRUCTs in
	//                    the same tx (true EIP-6780 deletion).
	//   calldata[0]==1 : STATICCALL the flag holder; if flag==0, CREATE2-redeploy tgt.
	//   calldata[0]==2 : f.slot0 = EXTCODEHASH(tgt)   (the victim read)
	f := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	fCode := common.FromHex("36156100835760003560f81c806002146100e85750602060006000600061bbbb5afa506000511561002c57005b6060600053600260015360616002536000600353600d600453606060055360006006536039600753606060085360026009536060600a536000600b5360f3600c536033600d5360ff600e536000600f60006000f550005b6060600053600260015360616002536000600353600d600453606060055360006006536039600753606060085360026009536060600a536000600b5360f3600c536033600d5360ff600e536000600f60006000f560006000600060006000855af15050005b5073fd0b03f591d1562409b6137e14ff608420f4e9463f60005500")

	// c: flag holder. Empty calldata returns slot0; non-empty gas-burns (so it
	// finishes last) and sets slot0 = 1.
	c := common.HexToAddress("0x000000000000000000000000000000000000bbbb")
	cCode := common.FromHex("366100105760005460005260206000f35b620138805b600190038060155750600160005500")

	cfg := *params.MergedTestChainConfig
	signer := types.NewLondonSigner(cfg.ChainID)
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0x00000000000000000000000000000000c0ffee00"),
		GasLimit:    30_000_000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(7),
		Random:      &common.Hash{},
	}

	buildRoot := func(tdb *triedb.Database, keys []*ecdsa.PrivateKey) common.Hash {
		gen, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
		gen.SetCode(f, fCode, tracing.CodeChangeUnspecified)
		gen.SetCode(c, cCode, tracing.CodeChangeUnspecified)
		for _, k := range keys {
			gen.AddBalance(crypto.PubkeyToAddress(k.PublicKey), uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
		}
		root, _ := gen.Commit(0, false, false)
		tdb.Commit(root, false)
		return root
	}

	mkTxs := func(keys []*ecdsa.PrivateKey) ([]*types.Transaction, []*Message) {
		mk := func(keyIdx int, to common.Address, data []byte) (*types.Transaction, *Message) {
			tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
				ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(0),
				GasFeeCap: big.NewInt(7), Gas: 5_000_000, To: &to, Value: big.NewInt(0), Data: data,
			}), signer, keys[keyIdx])
			if err != nil {
				t.Fatal(err)
			}
			msg, err := TransactionToMessage(tx, signer, blockCtx.BaseFee)
			if err != nil {
				t.Fatal(err)
			}
			return tx, msg
		}
		// One sender per tx: a shared sender would re-execute every later tx on
		// each re-exec (nonce/balance dependency), masking the bug.
		txs := make([]*types.Transaction, 4)
		msgs := make([]*Message, 4)
		txs[0], msgs[0] = mk(0, f, nil)       // create + selfdestruct tgt
		txs[1], msgs[1] = mk(1, c, []byte{1}) // heavy: set flag = 1 (finishes last)
		txs[2], msgs[2] = mk(2, f, []byte{1}) // redeploy tgt iff flag still reads 0
		txs[3], msgs[3] = mk(3, f, []byte{2}) // victim: f.slot0 = EXTCODEHASH(tgt)
		return txs, msgs
	}

	// BlockSTM scheduling is nondeterministic; iterate so a regression is caught
	// with overwhelming probability rather than relying on a single schedule.
	const iters = 8
	for i := range iters {
		memdb := rawdb.NewMemoryDatabase()
		tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
		keys := make([]*ecdsa.PrivateKey, 4)
		for j := range keys {
			keys[j], _ = crypto.GenerateKey()
		}
		root := buildRoot(tdb, keys)
		txs, msgs := mkTxs(keys)

		// Serial.
		serialDB, _ := state.New(root, state.NewDatabase(tdb, nil))
		gp := NewGasPool(blockCtx.GasLimit)
		serialEVM := vm.NewEVM(blockCtx, serialDB, &cfg, vm.Config{})
		for j, tx := range txs {
			serialDB.SetTxContext(tx.Hash(), j)
			if _, err := ApplyTransactionWithEVM(msgs[j], gp, serialDB, blockCtx.BlockNumber,
				common.Hash{}, blockCtx.Time, tx, serialEVM); err != nil {
				t.Fatalf("iter %d serial tx %d: %v", i, j, err)
			}
		}

		// V2 BlockSTM.
		v2DB, _ := state.New(root, state.NewDatabase(tdb, nil))
		base := v2DB.Copy()
		base.EnableConcurrentReads()
		tasks := make([]V2Task, len(txs))
		for j := range txs {
			tasks[j] = V2Task{Index: j, Tx: txs[j], Msg: msgs[j]}
		}
		ExecuteV2BlockSTM(context.Background(), tasks, base,
			blockstm.NewMVStore(), blockstm.NewMVBalanceStore(),
			blockCtx, common.Hash{}, vm.Config{}, &cfg, blockCtx.GasLimit, 4, v2DB, nil)
		v2DB.Finalise(true)

		if s, v := serialDB.IntermediateRoot(true), v2DB.IntermediateRoot(true); s != v {
			t.Fatalf("iter %d: V2 state root %s diverges from serial %s for metamorphic CREATE2 block", i, v.Hex(), s.Hex())
		}
	}
}
