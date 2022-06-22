package core

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// chainValidatorFake is a mock for the chain validator service
type chainValidatorFake struct {
	validate func(currentHeader *types.Header, chain []*types.Header) bool
}

// chainReaderFake is a mock for the chain reader service
type chainReaderFake struct {
	getTd func(hash common.Hash, number uint64) *big.Int
}

func newChainValidatorFake(validate func(currentHeader *types.Header, chain []*types.Header) bool) *chainValidatorFake {
	return &chainValidatorFake{validate: validate}
}

func newChainReaderFake(getTd func(hash common.Hash, number uint64) *big.Int) *chainReaderFake {
	return &chainReaderFake{getTd: getTd}
}

func TestCorrectPastChain(t *testing.T) {
	var (
		db      = rawdb.NewMemoryDatabase()
		genesis = (&Genesis{BaseFee: big.NewInt(params.InitialBaseFee)}).MustCommit(db)
	)

	hc, err := NewHeaderChain(db, params.AllEthashProtocolChanges, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	// Create mocks for forker
	getTd := func(hash common.Hash, number uint64) *big.Int {
		td := big.NewInt(0)
		if number == 64 {
			td = big.NewInt(64)
		}
		fmt.Println("Getting td", "hash", hash, "number", number, "td", td)
		return td
	}
	validate := func(currentHeader *types.Header, chain []*types.Header) bool {
		// Put all explicit conditions here
		// If canonical chain is empty and we're importing a chain of 64 blocks
		if currentHeader.Number.Uint64() == uint64(0) && len(chain) == 64 {
			fmt.Println("1st condition to validate")
			return true
		}
		// If canonical chain is of len 64 and we're importing a past chain from 54-64, then accept it
		if currentHeader.Number.Uint64() == uint64(64) && chain[0].Number.Uint64() == 55 && len(chain) == 10 {
			fmt.Println("2nd condition to validate")
			return true
		}
		return false
	}
	mockChainReader := newChainReaderFake(getTd)
	mockChainValidator := newChainValidatorFake(validate)
	mockForker := NewForkChoice(mockChainReader, nil, mockChainValidator)

	// chain A: G->A1->A2...A64
	chainA := makeHeaderChain(genesis.Header(), 64, ethash.NewFaker(), db, 10)
	fmt.Println("chainA: start", chainA[0].Number.Uint64(), "end", chainA[len(chainA)-1].Number.Uint64())
	fmt.Println("chainA: end block hash", chainA[len(chainA)-1].Hash())

	// Inserting 64 headers on an empty chain with a mock forker,
	// expecting 1 canon-status, 0 sidestatus with no error
	testInsert(t, hc, chainA, CanonStatTy, nil, mockForker)

	// chain B: G->A1->A2...A54->B55->B56...B64
	chainB := makeHeaderChain(chainA[53], 10, ethash.NewFaker(), db, 10)
	fmt.Println("chainB: start", chainB[0].Number.Uint64(), "end", chainB[len(chainB)-1].Number.Uint64())
	fmt.Println("chainB: end block hash", chainB[len(chainB)-1].Hash())

	getTd = func(hash common.Hash, number uint64) *big.Int {
		td := big.NewInt(0)
		if number == 64 {
			td = big.NewInt(64)
		}
		if hash == chainB[len(chainB)-1].Hash() {
			fmt.Println("here...")
			td = big.NewInt(65)
		}
		fmt.Println("Getting td", "hash", hash, "number", number, "td", td)
		return td
	}
	mockChainReader = newChainReaderFake(getTd)
	mockForker = NewForkChoice(mockChainReader, nil, mockChainValidator)

	testInsert(t, hc, chainB, CanonStatTy, nil, mockForker)

	fmt.Println("header block number", hc.CurrentHeader().Number.Uint64(), "header block hash", hc.CurrentHeader().Hash())
}

func TestCorrectFutureChain(t *testing.T) {
	var (
		db      = rawdb.NewMemoryDatabase()
		genesis = (&Genesis{BaseFee: big.NewInt(params.InitialBaseFee)}).MustCommit(db)
	)

	hc, err := NewHeaderChain(db, params.AllEthashProtocolChanges, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	// Create mocks for forker
	getTd := func(hash common.Hash, number uint64) *big.Int {
		td := big.NewInt(int64(number))
		// fmt.Println("Getting td", "hash", hash, "number", number, "td", td)
		return td
	}
	validate := func(currentHeader *types.Header, chain []*types.Header) bool {
		// Put all explicit conditions here
		// If canonical chain is empty and we're importing a chain of 64 blocks
		if currentHeader.Number.Uint64() == uint64(0) && len(chain) == 64 {
			fmt.Println("1st condition to validate")
			return true
		}
		// If canonical chain is of len 64 and we're importing a past chain from 64-74, then accept it
		// Here we mock the condition where future chains > some value should not be accepted
		if currentHeader.Number.Uint64() == uint64(64) && len(chain) <= 10 {
			fmt.Println("2nd condition to validate")
			return true
		}
		return false
	}
	mockChainReader := newChainReaderFake(getTd)
	mockChainValidator := newChainValidatorFake(validate)
	mockForker := NewForkChoice(mockChainReader, nil, mockChainValidator)

	// chain A: G->A1->A2...A64
	chainA := makeHeaderChain(genesis.Header(), 64, ethash.NewFaker(), db, 10)
	fmt.Println("chainA: start", chainA[0].Number.Uint64(), "end", chainA[len(chainA)-1].Number.Uint64(), "len", len(chainA))
	fmt.Println("chainA: end block hash", chainA[len(chainA)-1].Hash())

	// Inserting 64 headers on an empty chain with a mock forker,
	// expecting 1 canon-status, 0 sidestatus with no error
	testInsert(t, hc, chainA, CanonStatTy, nil, mockForker)

	// chain B: G->A1->A2...A64->B65->B66...B74
	chainB := makeHeaderChain(chainA[63], 10, ethash.NewFaker(), db, 10)
	fmt.Println("chainB: start", chainB[0].Number.Uint64(), "end", chainB[len(chainB)-1].Number.Uint64(), "len", len(chainB))
	fmt.Println("chainB: end block hash", chainB[len(chainB)-1].Hash())

	testInsert(t, hc, chainB, CanonStatTy, nil, mockForker)

	fmt.Println("header block number", hc.CurrentHeader().Number.Uint64(), "header block hash", hc.CurrentHeader().Hash())
}

func TestIncorrectPastChain(t *testing.T) {
	// TODO: decide if we need a separate test or add this
	// into the above test
}

func TestIncorrectFutureChain(t *testing.T) {
	var (
		db      = rawdb.NewMemoryDatabase()
		genesis = (&Genesis{BaseFee: big.NewInt(params.InitialBaseFee)}).MustCommit(db)
	)

	hc, err := NewHeaderChain(db, params.AllEthashProtocolChanges, ethash.NewFaker(), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	// Create mocks for forker
	getTd := func(hash common.Hash, number uint64) *big.Int {
		td := big.NewInt(int64(number))
		// fmt.Println("Getting td", "hash", hash, "number", number, "td", td)
		return td
	}
	validate := func(currentHeader *types.Header, chain []*types.Header) bool {
		// Put all explicit conditions here
		// If canonical chain is empty and we're importing a chain of 64 blocks
		if currentHeader.Number.Uint64() == uint64(0) && len(chain) == 64 {
			fmt.Println("1st condition to validate")
			return true
		}
		// If canonical chain is of len 64 and we're importing a past chain from 64-74, then accept it
		// Here we mock the condition where future chains > some value should not be accepted
		if currentHeader.Number.Uint64() == uint64(64) && len(chain) <= 10 {
			fmt.Println("2nd condition to validate")
			return true
		}
		fmt.Println("returning false in validate")
		return false
	}
	mockChainReader := newChainReaderFake(getTd)
	mockChainValidator := newChainValidatorFake(validate)
	mockForker := NewForkChoice(mockChainReader, nil, mockChainValidator)

	// chain A: G->A1->A2...A64
	chainA := makeHeaderChain(genesis.Header(), 64, ethash.NewFaker(), db, 10)
	fmt.Println("chainA: start", chainA[0].Number.Uint64(), "end", chainA[len(chainA)-1].Number.Uint64(), "len", len(chainA))
	fmt.Println("chainA: end block hash", chainA[len(chainA)-1].Hash())

	// Inserting 64 headers on an empty chain with a mock forker,
	// expecting 1 canon-status, 0 sidestatus with no error
	testInsert(t, hc, chainA, CanonStatTy, nil, mockForker)

	// chain B: G->A1->A2...A64->B65->B66...B84
	chainB := makeHeaderChain(chainA[63], 20, ethash.NewFaker(), db, 10)
	fmt.Println("chainB: start", chainB[0].Number.Uint64(), "end", chainB[len(chainB)-1].Number.Uint64(), "len", len(chainB))
	fmt.Println("chainB: end block hash", chainB[len(chainB)-1].Hash())

	testInsert(t, hc, chainB, SideStatTy, nil, mockForker)

	fmt.Println("header block number", hc.CurrentHeader().Number.Uint64(), "header block hash", hc.CurrentHeader().Hash())
}

// Mock chain reader functions
func (c *chainReaderFake) Config() *params.ChainConfig {
	return &params.ChainConfig{TerminalTotalDifficulty: nil}
}
func (c *chainReaderFake) GetTd(hash common.Hash, number uint64) *big.Int {
	return c.getTd(hash, number)
}

// Mock chain validator functions
func (w *chainValidatorFake) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	return true, nil
}
func (w *chainValidatorFake) IsValidChain(current *types.Header, headers []*types.Header) bool {
	return w.validate(current, headers)
}
func (w *chainValidatorFake) ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash) {}
func (w *chainValidatorFake) GetCheckpointWhitelist() map[uint64]common.Hash {
	return nil
}
func (w *chainValidatorFake) PurgeCheckpointWhitelist() {}
func (w *chainValidatorFake) GetCheckpoints(current, sidechainHeader *types.Header, sidechainCheckpoints []*types.Header) (map[uint64]*types.Header, error) {
	return map[uint64]*types.Header{}, nil
}
