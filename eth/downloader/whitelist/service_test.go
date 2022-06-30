package whitelist

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"gotest.tools/assert"
)

// NewMockService creates a new mock whitelist service
func NewMockService(maxCapacity uint, checkpointInterval uint64) *Service {
	return &Service{
		checkpointWhitelist: make(map[uint64]common.Hash),
		checkpointOrder:     []uint64{},
		maxCapacity:         maxCapacity,
		checkpointInterval:  checkpointInterval,
	}
}

// TestWhitelistCheckpoint checks the checkpoint whitelist map queue mechanism
func TestWhitelistCheckpoint(t *testing.T) {
	t.Parallel()

	s := NewMockService(10, 10)
	for i := 0; i < 10; i++ {
		s.enqueueCheckpointWhitelist(uint64(i), common.Hash{})
	}
	assert.Equal(t, s.length(), 10, "expected 10 items in whitelist")

	s.enqueueCheckpointWhitelist(11, common.Hash{})
	s.dequeueCheckpointWhitelist()
	assert.Equal(t, s.length(), 10, "expected 10 items in whitelist")
}

// TestIsValidPeer checks the IsValidPeer function in isolation
// for different cases by providing a mock fetchHeadersByNumber function
func TestIsValidPeer(t *testing.T) {
	t.Parallel()

	s := NewMockService(10, 10)

	// case1: no checkpoint whitelist, should consider the chain as valid
	res, err := s.IsValidPeer(nil, nil)
	assert.NilError(t, err, "expected no error")
	assert.Equal(t, res, true, "expected chain to be valid")

	// add checkpoint entries and mock fetchHeadersByNumber function
	s.ProcessCheckpoint(uint64(0), common.Hash{})
	s.ProcessCheckpoint(uint64(1), common.Hash{})

	assert.Equal(t, s.length(), 2, "expected 2 items in whitelist")

	// create a false function, returning absolutely nothing
	falseFetchHeadersByNumber := func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error) {
		return nil, nil, nil
	}

	// case2: false fetchHeadersByNumber function provided, should consider the chain as invalid
	// and throw `ErrNoRemoteCheckoint` error
	res, err = s.IsValidPeer(nil, falseFetchHeadersByNumber)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, ErrNoRemoteCheckoint) {
		t.Fatalf("expected error ErrNoRemoteCheckoint, got %v", err)
	}

	assert.Equal(t, res, false, "expected chain to be invalid")

	// case3: correct fetchHeadersByNumber function provided, should consider the chain as valid
	// create a mock function, returning a the required header
	fetchHeadersByNumber := func(number uint64, _ int, _ int, _ bool) ([]*types.Header, []common.Hash, error) {
		hash := common.Hash{}
		header := types.Header{Number: big.NewInt(0)}

		switch number {
		case 0:
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 1:
			header.Number = big.NewInt(1)
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 2:
			header.Number = big.NewInt(1) // sending wrong header for misamatch
			return []*types.Header{&header}, []common.Hash{hash}, nil
		default:
			return nil, nil, errors.New("invalid number")
		}
	}

	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	assert.NilError(t, err, "expected no error")
	assert.Equal(t, res, true, "expected chain to be valid")

	// add one more checkpoint whitelist entry
	s.ProcessCheckpoint(uint64(2), common.Hash{})
	assert.Equal(t, s.length(), 3, "expected 3 items in whitelist")

	// case4: correct fetchHeadersByNumber function provided with wrong header
	// for block number 2. Should consider the chain as invalid and throw an error
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	assert.Equal(t, err, ErrCheckpointMismatch, "expected checkpoint mismatch error")
	assert.Equal(t, res, false, "expected chain to be invalid")
}

// TestIsValidChain checks the IsValidChain function in isolation
// for different cases by providing a mock current header and chain
func TestIsValidChain(t *testing.T) {
	t.Parallel()

	s := NewMockService(10, 10)
	chainA := createMockChain(1, 20) // A1->A2...A19->A20
	// case1: no checkpoint whitelist, should consider the chain as valid
	res := s.IsValidChain(nil, chainA)
	assert.Equal(t, res, true, "expected chain to be valid")

	tempChain := createMockChain(21, 22) // A21->A22

	// add mock checkpoint entries
	s.ProcessCheckpoint(tempChain[0].Number.Uint64(), tempChain[0].Hash())
	s.ProcessCheckpoint(tempChain[1].Number.Uint64(), tempChain[1].Hash())

	assert.Equal(t, s.length(), 2, "expected 2 items in whitelist")

	// case2: We're behind the oldest whitelisted block entry, should consider
	// the chain as valid as we're still far behind the latest blocks
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	assert.Equal(t, res, true, "expected chain to be valid")

	// Clear checkpoint whitelist and add blocks A5 and A15 in whitelist
	s.PurgeCheckpointWhitelist()
	s.ProcessCheckpoint(chainA[5].Number.Uint64(), chainA[5].Hash())
	s.ProcessCheckpoint(chainA[15].Number.Uint64(), chainA[15].Hash())

	assert.Equal(t, s.length(), 2, "expected 2 items in whitelist")

	// case3: Try importing a past chain having valid checkpoint, should
	// consider the chain as valid
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	assert.Equal(t, res, true, "expected chain to be valid")

	// Clear checkpoint whitelist and mock blocks in whitelist
	tempChain = createMockChain(20, 20) // A20
	s.PurgeCheckpointWhitelist()
	s.ProcessCheckpoint(tempChain[0].Number.Uint64(), tempChain[0].Hash())

	assert.Equal(t, s.length(), 1, "expected 1 items in whitelist")

	// case4: Try importing a past chain having invalid checkpoint
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	assert.Equal(t, res, false, "expected chain to be invalid")

	// create a future chain to be imported of length <= `checkpointInterval`
	chainB := createMockChain(21, 30) // B21->B22...B29->B30

	// case5: Try importing a future chain of acceptable length
	res = s.IsValidChain(chainA[len(chainA)-1], chainB)
	assert.Equal(t, res, true, "expected chain to be valid")

	// create a future chain to be imported of length > `checkpointInterval`
	chainB = createMockChain(21, 40) // C21->C22...C39->C40

	// case5: Try importing a future chain of unacceptable length
	res = s.IsValidChain(chainA[len(chainA)-1], chainB)
	assert.Equal(t, res, false, "expected chain to be invalid")
}

func TestSplitChain(t *testing.T) {
	t.Parallel()

	type Result struct {
		pastStart    uint64
		pastEnd      uint64
		futureStart  uint64
		futureEnd    uint64
		pastLength   int
		futureLength int
	}

	// Current chain is at block: X
	// Incoming chain is represented as [N, M]
	testCases := []struct {
		name    string
		current uint64
		chain   []*types.Header
		result  Result
	}{
		{name: "X = 10, N = 11, M = 20", current: uint64(10), chain: createMockChain(11, 20), result: Result{futureStart: 11, futureEnd: 20, futureLength: 10}},
		{name: "X = 10, N = 13, M = 20", current: uint64(10), chain: createMockChain(13, 20), result: Result{futureStart: 13, futureEnd: 20, futureLength: 8}},
		{name: "X = 10, N = 2, M = 10", current: uint64(10), chain: createMockChain(2, 10), result: Result{pastStart: 2, pastEnd: 10, pastLength: 9}},
		{name: "X = 10, N = 2, M = 9", current: uint64(10), chain: createMockChain(2, 9), result: Result{pastStart: 2, pastEnd: 9, pastLength: 8}},
		{name: "X = 10, N = 2, M = 8", current: uint64(10), chain: createMockChain(2, 8), result: Result{pastStart: 2, pastEnd: 8, pastLength: 7}},
		{name: "X = 10, N = 5, M = 15", current: uint64(10), chain: createMockChain(5, 15), result: Result{pastStart: 5, pastEnd: 10, pastLength: 6, futureStart: 11, futureEnd: 15, futureLength: 5}},
		{name: "X = 10, N = 10, M = 20", current: uint64(10), chain: createMockChain(10, 20), result: Result{pastStart: 10, pastEnd: 10, pastLength: 1, futureStart: 11, futureEnd: 20, futureLength: 10}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			past, future := splitChain(tc.current, tc.chain)
			assert.Equal(t, len(past), tc.result.pastLength)
			assert.Equal(t, len(future), tc.result.futureLength)
			if len(past) > 0 {
				// Check if we have expected block
				assert.Equal(t, past[0].Number.Uint64(), tc.result.pastStart)
				assert.Equal(t, past[len(past)-1].Number.Uint64(), tc.result.pastEnd)

				// Check if the chain is ordered
				for i := 1; i < len(past); i++ {
					assert.Equal(t, past[i].Number.Uint64(), past[i-1].Number.Uint64()+1)
				}
			}
			if len(future) > 0 {
				// Check if we have expected block
				assert.Equal(t, future[0].Number.Uint64(), tc.result.futureStart)
				assert.Equal(t, future[len(future)-1].Number.Uint64(), tc.result.futureEnd)

				// Check if the chain is ordered
				for i := 1; i < len(future); i++ {
					assert.Equal(t, future[i].Number.Uint64(), future[i-1].Number.Uint64()+1)
				}
			}

			// Check if both chains are continuous
			if len(past) > 0 && len(future) > 0 {
				assert.Equal(t, past[len(past)-1].Number.Uint64(), future[0].Number.Uint64()-1)
			}
		})
	}
}

// createMockChain returns a chain with dummy headers
// starting from `start` to `end` (inclusive)
func createMockChain(start, end uint64) []*types.Header {
	var i uint64
	var chain []*types.Header
	for i = start; i <= end; i++ {
		header := &types.Header{
			Number: big.NewInt(int64(i)),
			Time:   uint64(time.Now().UnixMicro()) + i,
		}
		chain = append(chain, header)
	}
	return chain
}
