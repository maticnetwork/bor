package core

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// Addresses for the speculative-branch fixture. probeCold is the account only a
// speculative incarnation ever touches; probeWarm is the one the committed
// incarnation touches.
var (
	branchContract = common.HexToAddress("0x00000000000000000000000000000000000c0de0")
	probeCold      = common.HexToAddress("0x000000000000000000000000000000000000c01d")
	probeWarm      = common.HexToAddress("0x000000000000000000000000000000000000117a")
)

// branchContractCode dispatches on calldata presence:
//
//	no calldata  -> SSTORE(0, 1)                       (the writer, tx0)
//	any calldata -> v = SLOAD(0)                       (the brancher, tx1)
//	                v == 0 ? BALANCE(probeCold)
//	                       : BALANCE(probeWarm)
//
// The branch target is what makes this fixture adversarial. Both transactions
// conflict on slot 0, so tx1 is invalidated whenever it reads slot 0 before tx0
// commits -- and on that speculative pass it reads a *different account* than
// the incarnation that finally commits. probeCold's account-trie leaf is
// therefore reachable only through a discarded incarnation: no committed
// execution touches it, and no other transaction in the block does either.
func branchContractCode() []byte {
	asm := "36" + // CALLDATASIZE
		"6009" + // PUSH1 0x09   (read path)
		"57" + // JUMPI
		"6001" + // PUSH1 0x01
		"5f" + // PUSH0
		"55" + // SSTORE(0, 1)
		"00" + // STOP
		"5b" + // 0x09 JUMPDEST  (read path)
		"5f" + // PUSH0
		"54" + // SLOAD(0)
		"6027" + // PUSH1 0x27   (warm path)
		"57" + // JUMPI
		"73" + probeCold.Hex()[2:] + // PUSH20 probeCold
		"31" + // BALANCE
		"50" + // POP
		"00" + // STOP
		"5b" + // 0x27 JUMPDEST  (warm path)
		"73" + probeWarm.Hex()[2:] + // PUSH20 probeWarm
		"31" + // BALANCE
		"50" + // POP
		"00" // STOP

	return common.FromHex(asm)
}

// speculativeFixture builds the two-transaction block described above and
// returns everything needed to replay it.
type speculativeFixture struct {
	root     common.Hash
	tdb      *triedb.Database
	tasks    []V2Task
	blockCtx vm.BlockContext
	config   *params.ChainConfig
	header   *types.Header
}

func newSpeculativeFixture(t *testing.T) *speculativeFixture {
	t.Helper()

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)

	gen, err := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatalf("genesis state: %v", err)
	}

	cfg := *params.MergedTestChainConfig

	gen.SetCode(branchContract, branchContractCode(), tracing.CodeChangeUnspecified)

	// Both probes must exist, or BALANCE resolves no trie node and the
	// divergence this fixture is built to expose cannot be observed.
	gen.AddBalance(probeCold, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
	gen.AddBalance(probeWarm, uint256.NewInt(2), tracing.BalanceChangeUnspecified)

	// Distinct senders: two transactions from one sender carry a nonce
	// dependency that would serialize them, and a serialized pair never
	// speculates.
	writerKey, _ := crypto.GenerateKey()
	brancherKey, _ := crypto.GenerateKey()
	writer := crypto.PubkeyToAddress(writerKey.PublicKey)
	brancher := crypto.PubkeyToAddress(brancherKey.PublicKey)
	gen.AddBalance(writer, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
	gen.AddBalance(brancher, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)

	root, err := gen.Commit(0, false, false)
	if err != nil {
		t.Fatalf("commit genesis: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("commit triedb: %v", err)
	}

	signer := types.NewLondonSigner(cfg.ChainID)

	writerTx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(7),
		Gas: 200_000, To: &branchContract, Value: big.NewInt(0),
	}), signer, writerKey)
	if err != nil {
		t.Fatalf("sign writer tx: %v", err)
	}

	brancherTx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(7),
		Gas: 200_000, To: &branchContract, Value: big.NewInt(0), Data: []byte{0x01},
	}), signer, brancherKey)
	if err != nil {
		t.Fatalf("sign brancher tx: %v", err)
	}

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

	writerMsg, err := TransactionToMessage(writerTx, signer, blockCtx.BaseFee)
	if err != nil {
		t.Fatalf("writer msg: %v", err)
	}
	brancherMsg, err := TransactionToMessage(brancherTx, signer, blockCtx.BaseFee)
	if err != nil {
		t.Fatalf("brancher msg: %v", err)
	}

	return &speculativeFixture{
		root: root,
		tdb:  tdb,
		tasks: []V2Task{
			{Index: 0, Tx: writerTx, Msg: writerMsg},
			{Index: 1, Tx: brancherTx, Msg: brancherMsg},
		},
		blockCtx: blockCtx,
		config:   &cfg,
		header: &types.Header{
			Number:   new(big.Int).Set(blockCtx.BlockNumber),
			Time:     blockCtx.Time,
			GasLimit: blockCtx.GasLimit,
			BaseFee:  new(big.Int).Set(blockCtx.BaseFee),
			Root:     root,
		},
	}
}

// run replays the fixture through V2 at the given width and returns the
// witness it produced, over the production shared-reader stack so read-set
// collection behaves the way it does in a node.
func (f *speculativeFixture) run(workers int) (*stateless.Witness, *V2ExecutionResult, error) {
	db := state.NewDatabase(f.tdb, nil)

	_, _, parallelReader, err := db.ReadersWithCacheStatsTriple(f.root)
	if err != nil {
		return nil, nil, fmt.Errorf("readers: %w", err)
	}

	finalDB, err := state.NewWithReader(f.root, db, parallelReader)
	if err != nil {
		return nil, nil, fmt.Errorf("state: %w", err)
	}

	w, err := stateless.NewWitness(f.header, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("witness: %w", err)
	}
	finalDB.SetWitness(w)

	readBase := finalDB.Copy()
	readBase.EnableConcurrentReads()
	// Copy() deep-copies the witness; re-share so worker-side adds land in w.
	readBase.SetWitness(w)

	res := ExecuteV2BlockSTM(context.Background(), f.tasks, readBase,
		blockstm.NewMVStore(), blockstm.NewMVBalanceStore(), f.blockCtx, common.Hash{},
		vm.Config{}, f.config, f.blockCtx.GasLimit, workers, finalDB, nil)

	// Same order production uses: drain the read set, then compute the root so
	// update-path nodes are recorded too.
	finalDB.CollectStateWitness()
	finalDB.IntermediateRoot(f.config.IsEIP158(f.blockCtx.BlockNumber))

	return w, res, nil
}

// TestV2WitnessSpeculativeBranchDivergence drives the exact shape that makes
// V2 witness collection scheduling-dependent: a transaction whose *access set*
// -- not merely its values -- changes between the incarnation that is thrown
// away and the one that commits.
//
// Witness collection has no incarnation filter. SafeBase.CollectCodeWitness
// ranges the whole shared code cache, and readerWithCache.claimUnwalkedItems
// walks every cached account and slot into the trie; nothing prunes either on
// invalidation. So a read performed only by a discarded incarnation is still
// recorded, and whether that read happened at all depends on worker timing.
//
// If the produced witnesses differ, two honest nodes publish different bytes
// for the same block and WIT2's BP-signed commitment stops identifying the
// artifact it is supposed to identify.
func TestV2WitnessSpeculativeBranchDivergence(t *testing.T) {
	f := newSpeculativeFixture(t)

	seen := map[common.Hash][]specOutcome{}

	// Width 1 forces index order (no speculation); wider runs let tx1 read
	// slot 0 before tx0 commits. Repeats catch the race either way round.
	for _, workers := range []int{1, 1, 2, 4, 8, 8, 16, 16} {
		for attempt := 0; attempt < 4; attempt++ {
			w, res, err := f.run(workers)
			if err != nil {
				t.Fatalf("workers=%d: %v", workers, err)
			}

			h, err := stateless.WitnessCommitHashFromWitness(w)
			if err != nil {
				t.Fatalf("hashing witness: %v", err)
			}

			o := specOutcome{workers: workers, hash: h, nodes: len(w.State), codes: len(w.Codes)}
			if res != nil {
				o.execs = res.ExecCount
			}
			seen[h] = append(seen[h], o)
		}
	}

	for h, os := range seen {
		t.Logf("commit=%x runs=%-3d nodes=%-4d codes=%-3d widths=%v",
			h[:10], len(os), os[0].nodes, os[0].codes, widthsOf(os))
	}

	if len(seen) > 1 {
		t.Errorf("V2 produced %d distinct witnesses for the same block: witness content "+
			"depends on BlockSTM worker scheduling, so two honest nodes disagree on the bytes", len(seen))
	}
}

// specOutcome is one replay of the speculative fixture.
type specOutcome struct {
	workers int
	hash    common.Hash
	nodes   int
	codes   int
	execs   int
}

func widthsOf(os []specOutcome) []int {
	out := make([]int, 0, len(os))
	seen := map[int]bool{}
	for _, o := range os {
		if !seen[o.workers] {
			seen[o.workers] = true
			out = append(out, o.workers)
		}
	}
	return out
}
