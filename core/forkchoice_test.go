package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// chainValidatorFake is a mock for the chain validator service
type chainValidatorFake struct {
	validate func(currentHeader *types.Header, chain []*types.Header) (bool, error)
}

// chainReaderFake is a mock for the chain reader service
type chainReaderFake struct {
	getTd func(hash common.Hash, number uint64) *big.Int
}

func newChainValidatorFake(validate func(currentHeader *types.Header, chain []*types.Header) (bool, error)) *chainValidatorFake {
	return &chainValidatorFake{validate: validate}
}

func newChainReaderFake(getTd func(hash common.Hash, number uint64) *big.Int) *chainReaderFake {
	return &chainReaderFake{getTd: getTd}
}

func TestPastChainInsert(t *testing.T) {
	t.Parallel()

	var (
		db    = rawdb.NewMemoryDatabase()
		gspec = &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: params.AllEthashProtocolChanges}
	)

	_, _ = gspec.Commit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	hc, err := NewHeaderChain(db, gspec.Config, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	// chain A: G->A1->A2...A64
	genDb, chainA := makeHeaderChainWithGenesis(gspec, 64, ethash.NewFaker(), 10)

	// Inserting 64 headers on an empty chain
	// expecting 1 write status with no error
	testInsert(t, hc, chainA, CanonStatTy, nil)

	// The current chain is: G->A1->A2...A64
	// chain B: G->A1->A2...A44->B45->B46...B64
	chainB := makeHeaderChain(gspec.Config, chainA[43], 20, ethash.NewFaker(), genDb, 10)

	// The current chain is: G->A1->A2...A64
	// chain C: G->A1->A2...A54->C55->C56...C64
	chainC := makeHeaderChain(gspec.Config, chainA[53], 10, ethash.NewFaker(), genDb, 10)

	// Inserting 20 blocks from chainC on canonical chain
	// expecting 2 write status with no error
	testInsert(t, hc, chainB, SideStatTy, nil)

	// Inserting 10 blocks from chainB on canonical chain
	// expecting 1 write status with no error
	testInsert(t, hc, chainC, CanonStatTy, nil)
}

func TestFutureChainInsert(t *testing.T) {
	t.Parallel()

	var (
		db    = rawdb.NewMemoryDatabase()
		gspec = &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: params.AllEthashProtocolChanges}
	)

	_, _ = gspec.Commit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	hc, err := NewHeaderChain(db, gspec.Config, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	// chain A: G->A1->A2...A64
	genDb, chainA := makeHeaderChainWithGenesis(gspec, 64, ethash.NewFaker(), 10)

	// Inserting 64 headers on an empty chain
	// expecting 1 write status with no error
	testInsert(t, hc, chainA, CanonStatTy, nil)

	// The current chain is: G->A1->A2...A64
	// chain B: G->A1->A2...A64->B65->B66...B84
	chainB := makeHeaderChain(gspec.Config, chainA[63], 20, ethash.NewFaker(), genDb, 10)

	// Inserting 20 headers on the canonical chain
	// expecting 0 write status with no error
	testInsert(t, hc, chainB, SideStatTy, nil)

	// The current chain is: G->A1->A2...A64
	// chain C: G->A1->A2...A64->C65->C66...C74
	chainC := makeHeaderChain(gspec.Config, chainA[63], 10, ethash.NewFaker(), genDb, 10)

	// Inserting 10 headers on the canonical chain
	// expecting 0 write status with no error
	testInsert(t, hc, chainC, CanonStatTy, nil)
}

func TestOverlappingChainInsert(t *testing.T) {
	t.Parallel()

	var (
		db    = rawdb.NewMemoryDatabase()
		gspec = &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: params.AllEthashProtocolChanges}
	)

	_, _ = gspec.Commit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	hc, err := NewHeaderChain(db, gspec.Config, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	// chain A: G->A1->A2...A64
	genDb, chainA := makeHeaderChainWithGenesis(gspec, 64, ethash.NewFaker(), 10)

	// Inserting 64 headers on an empty chain
	// expecting 1 write status with no error
	testInsert(t, hc, chainA, CanonStatTy, nil)

	// The current chain is: G->A1->A2...A64
	// chain B: G->A1->A2...A54->B55->B56...B84
	chainB := makeHeaderChain(gspec.Config, chainA[53], 30, ethash.NewFaker(), genDb, 10)

	// Inserting 20 blocks on canonical chain
	// expecting 2 write status with no error
	testInsert(t, hc, chainB, SideStatTy, nil)

	// The current chain is: G->A1->A2...A64
	// chain C: G->A1->A2...A54->C55->C56...C74
	chainC := makeHeaderChain(gspec.Config, chainA[53], 20, ethash.NewFaker(), genDb, 10)

	// Inserting 10 blocks on canonical chain
	// expecting 1 write status with no error
	testInsert(t, hc, chainC, CanonStatTy, nil)
}

// Mock chain reader functions
func (c *chainReaderFake) Config() *params.ChainConfig {
	return &params.ChainConfig{TerminalTotalDifficulty: nil}
}
func (c *chainReaderFake) GetTd(hash common.Hash, number uint64) *big.Int {
	return c.getTd(hash, number)
}

// Mock chain validator functions
func (w *chainValidatorFake) IsValidPeer(fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	return true, nil
}
func (w *chainValidatorFake) IsValidChain(current *types.Header, headers []*types.Header) (bool, error) {
	return w.validate(current, headers)
}
func (w *chainValidatorFake) ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash) {}
func (w *chainValidatorFake) ProcessMilestone(endBlockNum uint64, endBlockHash common.Hash)  {}
func (w *chainValidatorFake) ProcessFutureMilestone(num uint64, hash common.Hash) {
}
func (w *chainValidatorFake) GetWhitelistedCheckpoint() (bool, uint64, common.Hash) {
	return false, 0, common.Hash{}
}

func (w *chainValidatorFake) GetWhitelistedMilestone() (bool, uint64, common.Hash) {
	return false, 0, common.Hash{}
}
func (w *chainValidatorFake) PurgeWhitelistedCheckpoint() {}
func (w *chainValidatorFake) PurgeWhitelistedMilestone()  {}
func (w *chainValidatorFake) GetCheckpoints(current, sidechainHeader *types.Header, sidechainCheckpoints []*types.Header) (map[uint64]*types.Header, error) {
	return map[uint64]*types.Header{}, nil
}
func (w *chainValidatorFake) LockMutex(endBlockNum uint64) bool {
	return false
}
func (w *chainValidatorFake) UnlockMutex(doLock bool, milestoneId string, endBlockNum uint64, endBlockHash common.Hash) {
}
func (w *chainValidatorFake) UnlockSprint(endBlockNum uint64) {
}
func (w *chainValidatorFake) RemoveMilestoneID(milestoneId string) {
}
func (w *chainValidatorFake) GetMilestoneIDsList() []string {
	return nil
}
