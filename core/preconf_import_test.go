package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

func TestPipelinedPreconfLifecycleUsesInnerWritePath(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	statedb, err := chain.StateAt(chain.CurrentBlock().Root)
	require.NoError(t, err)
	provider := &testPreconfProvider{invalidation: "canonical_mismatch"}
	chain.SetPreconfProvider(provider)

	status, err := chain.WriteBlockAndSetHeadPipelined(blocks[0], nil, nil, statedb, false, nil)
	require.NoError(t, err)
	require.Equal(t, CanonStatTy, status)
	require.Equal(t, 1, provider.committed)
	require.Equal(t, []rawdb.InvalidPreconfRecord{{Number: blocks[0].NumberU64(), Reason: "canonical_mismatch"}}, rawdb.ReadInvalidPreconfs(chain.DB(), 1))
}

func TestPipelinedCanonicalImportCompletesObservedLifecycleOnce(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	provider := new(testPreconfProvider)
	chain.SetPreconfProvider(provider)

	if _, err := chain.InsertChain(types.Blocks{blocks[0]}, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if provider.begun != 1 || provider.begunBlock != blocks[0].Hash() || provider.committed != 1 || provider.failed != 0 {
		t.Fatalf("provider=%+v", provider)
	}
}

type testPreconfProvider struct {
	block           common.Hash
	execution       *PreconfExecution
	prefixBlock     common.Hash
	prefixExecution *PreconfExecution
	begun           int
	begunBlock      common.Hash
	claims          int
	prefixClaims    int
	committed       int
	failed          int
	invalidation    string
}

func (p *testPreconfProvider) BeginPreconfImport(block *types.Block) {
	p.begun++
	p.begunBlock = block.Hash()
}

func (p *testPreconfProvider) ClaimPreconfPrefix(block *types.Block) (*PreconfExecution, bool) {
	p.prefixClaims++
	if block.Hash() != p.prefixBlock || p.prefixExecution == nil {
		return nil, false
	}
	return &PreconfExecution{StateDB: p.prefixExecution.StateDB.Copy(), Result: p.prefixExecution.Result}, true
}

func (p *testPreconfProvider) ClaimPreconf(block *types.Block) (*PreconfExecution, bool) {
	p.claims++
	if block.Hash() != p.block || p.execution == nil {
		return nil, false
	}
	return &PreconfExecution{StateDB: p.execution.StateDB.Copy(), Result: p.execution.Result}, true
}

func (p *testPreconfProvider) RejectClaimedPreconf(block *types.Block) {
	p.failed++
}

func (p *testPreconfProvider) CompletePreconf(block *types.Block, receipts types.Receipts, committed bool) string {
	if committed {
		p.committed++
		return p.invalidation
	}
	p.failed++
	return ""
}

type countingProcessor struct {
	inner Processor
	calls int
}

type failingPreconfProcessor struct {
	err error
}

func (p *failingPreconfProcessor) Process(*types.Block, *state.StateDB, vm.Config, *common.Address, context.Context) (*ProcessResult, error) {
	return nil, p.err
}

func (p *countingProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, author *common.Address, interrupt context.Context) (*ProcessResult, error) {
	p.calls++
	return p.inner.Process(block, statedb, cfg, author, interrupt)
}

func TestCanonicalImportReusesPreconfExecution(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	block := blocks[0]
	receipts, logs, usedGas, statedb, _, err := blockchain.ProcessBlock(block, blockchain.GetHeaderByHash(block.ParentHash()), nil, nil, nil)
	if err != nil {
		t.Fatalf("pre-execute: %v", err)
	}
	result := &ProcessResult{Receipts: receipts, Logs: logs, GasUsed: usedGas}
	provider := &testPreconfProvider{
		block: block.Hash(),
		execution: &PreconfExecution{
			StateDB: statedb,
			Result:  result,
		},
	}
	blockchain.SetPreconfProvider(provider)
	counter := &countingProcessor{inner: blockchain.processor}
	blockchain.processor = counter
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if counter.calls != 0 || provider.claims != 1 || provider.committed != 1 || provider.failed != 0 {
		t.Fatalf("calls=%d provider=%+v", counter.calls, provider)
	}
}

func TestCanonicalImportFallsBackOnPreconfMiss(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	provider := &testPreconfProvider{}
	blockchain.SetPreconfProvider(provider)
	counter := &countingProcessor{inner: blockchain.processor}
	blockchain.processor = counter
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if counter.calls != 1 || provider.begun != 1 || provider.begunBlock != blocks[0].Hash() || provider.claims != 1 || provider.committed != 1 {
		t.Fatalf("calls=%d provider=%+v", counter.calls, provider)
	}
}

func TestCanonicalImportSignalsPreconfLifecycleWhenReuseDisabled(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	provider := new(testPreconfProvider)
	blockchain.SetPreconfProvider(provider)
	blockchain.cfg.VmConfig.EnablePreimageRecording = true
	counter := &countingProcessor{inner: blockchain.processor}
	blockchain.processor = counter
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if counter.calls != 1 || provider.begun != 1 || provider.begunBlock != blocks[0].Hash() ||
		provider.claims != 0 || provider.prefixClaims != 0 || provider.committed != 1 || provider.failed != 0 {
		t.Fatalf("calls=%d provider=%+v", counter.calls, provider)
	}
}

func TestStagedImportDefersPreconfInvalidation(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	provider := new(testPreconfProvider)
	blockchain.SetPreconfProvider(provider)
	if _, err := blockchain.InsertBlockWithoutSetHead(blocks[0], false); err != nil {
		t.Fatalf("staged insert: %v", err)
	}
	if provider.committed != 0 {
		t.Fatalf("staged import finalized preconfirmation: %+v", provider)
	}
	if provider.begun != 0 {
		t.Fatalf("staged import started canonical preconfirmation lifecycle: %+v", provider)
	}
}

func TestCanonicalImportReusesCompletedPreconfPrefix(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	block := blocks[0]
	receipts, logs, usedGas, statedb, _, err := blockchain.ProcessBlock(block, blockchain.GetHeaderByHash(block.ParentHash()), nil, nil, nil)
	if err != nil {
		t.Fatalf("pre-execute: %v", err)
	}
	provider := &testPreconfProvider{
		prefixBlock: block.Hash(),
		prefixExecution: &PreconfExecution{
			StateDB: statedb,
			Result:  &ProcessResult{Receipts: receipts, Logs: logs, GasUsed: usedGas},
		},
	}
	blockchain.SetPreconfProvider(provider)
	counter := &countingProcessor{inner: blockchain.processor}
	blockchain.processor = counter
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if counter.calls != 0 || provider.claims != 1 || provider.prefixClaims != 1 || provider.failed != 0 {
		t.Fatalf("calls=%d provider=%+v", counter.calls, provider)
	}
}

func TestCanonicalImportWithSuppliedWitnessSkipsPreconf(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	block := blocks[0]
	witness, err := stateless.NewWitness(block.Header(), blockchain)
	if err != nil {
		t.Fatalf("witness: %v", err)
	}
	provider := &testPreconfProvider{block: block.Hash(), prefixBlock: block.Hash()}
	blockchain.SetPreconfProvider(provider)
	counter := &countingProcessor{inner: blockchain.processor}
	blockchain.processor = counter
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChainWithWitnesses(blocks, false, []*stateless.Witness{witness}); err != nil {
		t.Fatalf("insert with witness: %v", err)
	}
	if counter.calls != 1 || provider.claims != 0 || provider.prefixClaims != 0 {
		t.Fatalf("calls=%d provider=%+v", counter.calls, provider)
	}
}

func TestCanonicalImportRejectsInvalidPreconfExecution(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	block := blocks[0]
	_, _, _, statedb, _, err := blockchain.ProcessBlock(block, blockchain.GetHeaderByHash(block.ParentHash()), nil, nil, nil)
	if err != nil {
		t.Fatalf("pre-execute: %v", err)
	}
	provider := &testPreconfProvider{
		block: block.Hash(),
		execution: &PreconfExecution{
			StateDB: statedb,
			Result:  &ProcessResult{GasUsed: block.GasUsed() + 1},
		},
	}
	blockchain.SetPreconfProvider(provider)
	counter := &countingProcessor{inner: blockchain.processor}
	blockchain.processor = counter
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if counter.calls != 1 || provider.failed != 1 || provider.committed != 1 {
		t.Fatalf("calls=%d provider=%+v", counter.calls, provider)
	}
}

func TestFailedFallbackReleasesRejectedPreconf(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	block := blocks[0]
	_, _, _, statedb, _, err := blockchain.ProcessBlock(block, blockchain.GetHeaderByHash(block.ParentHash()), nil, nil, nil)
	if err != nil {
		t.Fatalf("pre-execute: %v", err)
	}
	provider := &testPreconfProvider{
		block: block.Hash(),
		execution: &PreconfExecution{
			StateDB: statedb,
			Result:  &ProcessResult{GasUsed: block.GasUsed() + 1},
		},
	}
	blockchain.SetPreconfProvider(provider)
	processErr := errors.New("fallback failed")
	blockchain.processor = &failingPreconfProcessor{err: processErr}
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); !errors.Is(err, processErr) {
		t.Fatalf("insert error = %v, want %v", err, processErr)
	}
	if provider.begun != 1 || provider.failed != 2 || provider.committed != 0 {
		t.Fatalf("provider=%+v", provider)
	}
}

func TestFailedCanonicalImportReleasesPreconfLifecycle(t *testing.T) {
	engine := ethash.NewFaker()
	_, genesis, blockchain, err := newCanonical(engine, 0, true, "hash")
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	defer blockchain.Stop()

	_, blocks := makeBlockChainWithGenesis(genesis, 1, engine, canonicalSeed)
	provider := new(testPreconfProvider)
	blockchain.SetPreconfProvider(provider)
	blockchain.cfg.VmConfig.EnablePreimageRecording = true
	processErr := errors.New("canonical execution failed")
	blockchain.processor = &failingPreconfProcessor{err: processErr}
	blockchain.parallelProcessor = nil

	if _, err := blockchain.InsertChain(blocks, false); !errors.Is(err, processErr) {
		t.Fatalf("insert error = %v, want %v", err, processErr)
	}
	if provider.begun != 1 || provider.begunBlock != blocks[0].Hash() || provider.failed != 1 || provider.committed != 0 {
		t.Fatalf("provider=%+v", provider)
	}
}

func TestPreconfCompatibility(t *testing.T) {
	if !(&BlockChain{cfg: DefaultConfig()}).canClaimPreconf(false) {
		t.Fatal("default canonical import rejected preconfirmation reuse")
	}
	for _, test := range []struct {
		name    string
		witness bool
		mutate  func(*BlockChain)
	}{
		{name: "witness", witness: true, mutate: func(*BlockChain) {}},
		{name: "stateless", mutate: func(chain *BlockChain) { chain.cfg.Stateless = true }},
		{name: "enforced parallel", mutate: func(chain *BlockChain) { chain.enforceParallelProcessor = true }},
		{name: "tracer", mutate: func(chain *BlockChain) { chain.cfg.VmConfig.Tracer = &tracing.Hooks{} }},
		{name: "no base fee", mutate: func(chain *BlockChain) { chain.cfg.VmConfig.NoBaseFee = true }},
		{name: "preimages", mutate: func(chain *BlockChain) { chain.cfg.VmConfig.EnablePreimageRecording = true }},
		{name: "extra eip", mutate: func(chain *BlockChain) { chain.cfg.VmConfig.ExtraEips = []int{1153} }},
		{name: "self validation", mutate: func(chain *BlockChain) { chain.cfg.VmConfig.StatelessSelfValidation = true }},
		{name: "witness stats", mutate: func(chain *BlockChain) { chain.cfg.VmConfig.EnableWitnessStats = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain := &BlockChain{cfg: DefaultConfig()}
			test.mutate(chain)
			if chain.canClaimPreconf(test.witness) {
				t.Fatal("unsafe import mode accepted preconfirmation reuse")
			}
			provider := new(testPreconfProvider)
			chain.preconfProvider = provider
			chain.claimPreconf(types.NewBlockWithHeader(&types.Header{}), test.witness, true, true)
			if provider.claims != 0 || provider.prefixClaims != 0 {
				t.Fatalf("unsafe import mode attempted claims: full=%d prefix=%d", provider.claims, provider.prefixClaims)
			}
		})
	}
}
