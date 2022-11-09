package whitelist

import (
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// NewMockService creates a new mock whitelist service
func NewMockService() *WhitelistService {
	return &WhitelistService{

		checkpoint{
			doExist:  false,
			interval: 256,
		},

		milestone{
			doExist:            false,
			interval:           256,
			LockedMilestoneIds: make(map[string]bool),
		},
	}
}

// TestWhitelistCheckpoint checks the checkpoint whitelist setter and getter functions.
func TestWhitelistedCheckpoint(t *testing.T) {
	t.Parallel()

	//Creating the service for the whitelisting the checkpoints
	s := NewMockService()

	require.Equal(t, s.checkpoint.doExist, false, "expected false as no checkpoint exist at this point")

	//Adding the checkpoint
	s.ProcessCheckpoint(11, common.Hash{})

	require.Equal(t, s.checkpoint.doExist, true, "expected true as checkpoint exist")

	//Removing the checkpoint
	s.PurgeWhitelistedCheckpoint()

	require.Equal(t, s.checkpoint.doExist, false, "expected false as no checkpoint exist at this point")

	//Adding the checkpoint
	s.ProcessCheckpoint(11, common.Hash{1})

	//Receiving the stored checkpoint
	doExist, number, hash := s.GetWhitelistedCheckpoint()

	//Validating the values received
	require.Equal(t, doExist, true, "expected true ascheckpoint exist at this point")
	require.Equal(t, number, uint64(11), "expected number to be 11 but got", number)
	require.Equal(t, hash, common.Hash{1}, "expected the 1 hash but got", hash)
	require.NotEqual(t, hash, common.Hash{}, "expected the hash to be different from zero hash")
}

// TestMilestone checks the milestone whitelist setter and getter functions
func TestMilestone(t *testing.T) {
	t.Parallel()

	s := NewMockService()

	//Checking for the varaibles when no milestone is Processed
	require.Equal(t, s.milestone.doExist, false, "expected false as no milestone exist at this point")
	require.Equal(t, s.milestone.Locked, false, "expected false as it was not locked")
	require.Equal(t, s.milestone.LockedSprintNumber, uint64(0), "expected 0 as it was not initialized")

	//Acquiring the mutex lock
	s.LockMutex(11)
	require.Equal(t, s.milestone.LockedSprintNumber, uint64(11), "expected 11")
	require.Equal(t, s.milestone.Locked, false, "expected false as sprint is not locked till this point")

	//Releasing the mutex lock
	s.UnlockMutex(true, "milestoneID1", common.Hash{})
	require.Equal(t, s.milestone.LockedSprintNumber, uint64(11), "expected 11 as it was not initialized")
	require.Equal(t, s.milestone.Locked, true, "expected true as sprint is locked now")
	require.Equal(t, len(s.milestone.LockedMilestoneIds), int(1), "expected 1 as only 1 milestoneID has been entered")
	require.Equal(t, s.milestone.LockedMilestoneIds["milestoneID1"], true, "expected true as we have stored this milestoneID1 previously")
	require.Equal(t, s.milestone.LockedMilestoneIds["milestoneID2"], false, "expected false as we have not stored this milestoneID2 previously")

	s.LockMutex(11)
	s.UnlockMutex(true, "milestoneID2", common.Hash{})
	require.Equal(t, len(s.milestone.LockedMilestoneIds), int(2), "expected 1 as only 1 milestoneID has been entered")
	require.Equal(t, s.milestone.LockedMilestoneIds["milestoneID2"], true, "expected true as we have stored this milestoneID2 previously")

	s.RemoveMilestoneID("milestoneID1")
	require.Equal(t, len(s.milestone.LockedMilestoneIds), int(1), "expected 1 as one out of two has been removed in previous step")
	require.Equal(t, s.milestone.Locked, true, "expected true as sprint is locked now")

	s.RemoveMilestoneID("milestoneID2")
	require.Equal(t, len(s.milestone.LockedMilestoneIds), int(0), "expected 1 as both the milestonesIDs has been removed in previous step")
	require.Equal(t, s.milestone.Locked, false, "expected false")

	s.LockMutex(11)
	s.UnlockMutex(true, "milestoneID3", common.Hash{})
	require.True(t, s.milestone.Locked, "expected true")
	require.Equal(t, s.milestone.LockedSprintNumber, uint64(11), "Expected 11")

	s.LockMutex(15)
	require.False(t, s.milestone.Locked, "expected false as till now final confirmation regarding the lock hasn't been made")
	require.Equal(t, s.milestone.LockedSprintNumber, uint64(15), "Expected 15")
	s.UnlockMutex(true, "milestoneID4", common.Hash{})
	require.True(t, s.milestone.Locked, "expected true as final confirmation regarding the lock has been made")

	//Adding the milestone
	s.ProcessMilestone(11, common.Hash{})

	require.True(t, s.milestone.Locked, "expected true as locked sprint is of number 15")
	require.Equal(t, s.milestone.doExist, true, "expected true as milestone exist")
	require.Equal(t, len(s.milestone.LockedMilestoneIds), int(1), "expected 1 as still last milestone of sprint number 15 exist")

	//Asking the lock for sprintNumber less than last whitelisted milestone
	require.False(t, s.LockMutex(11), "Cant lock the sprintNumber less than equal to latest whitelisted milestone")
	s.UnlockMutex(false, "", common.Hash{}) //Unlock is required after every lock to release the mutex

	//Adding the milestone
	s.ProcessMilestone(51, common.Hash{})
	require.False(t, s.milestone.Locked, "expected false as lock from sprint number 15 is removed")
	require.Equal(t, s.milestone.doExist, true, "expected true as milestone exist")
	require.Equal(t, len(s.milestone.LockedMilestoneIds), int(0), "expected 0 as all the milestones have been removed")

	//Removing the milestone
	s.PurgeWhitelistedMilestone()

	require.Equal(t, s.milestone.doExist, false, "expected false as no milestone exist at this point")

	//Removing the milestone
	s.ProcessMilestone(11, common.Hash{1})

	doExist, number, hash := s.GetWhitelistedMilestone()

	//validating the values received
	require.Equal(t, doExist, true, "expected true as milestone exist at this point")
	require.Equal(t, number, uint64(11), "expected number to be 11 but got", number)
	require.Equal(t, hash, common.Hash{1}, "expected the 1 hash but got", hash)
}

// TestIsValidPeer checks the IsValidPeer function in isolation
// for different cases by providing a mock fetchHeadersByNumber function
func TestIsValidPeer(t *testing.T) {
	t.Parallel()

	s := NewMockService()

	// case1: no checkpoint whitelist, should consider the chain as valid
	res, err := s.IsValidPeer(nil, nil)
	require.NoError(t, err, "expected no error")
	require.Equal(t, res, true, "expected chain to be valid")

	// add checkpoint entry and mock fetchHeadersByNumber function
	s.ProcessCheckpoint(uint64(1), common.Hash{})

	// add milestone entry and mock fetchHeadersByNumber function
	s.ProcessMilestone(uint64(1), common.Hash{})

	//Check whether the milestone and checkpoint exist
	require.Equal(t, s.checkpoint.doExist, true, "expected true as checkpoint exists")
	require.Equal(t, s.milestone.doExist, true, "expected true as milestone exists")

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

	if !errors.Is(err, ErrNoRemote) {
		t.Fatalf("expected error ErrNoRemote, got %v", err)
	}

	require.Equal(t, res, false, "expected peer chain to be invalid")

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

	// case3: correct fetchHeadersByNumber function provided, should consider the chain as valid
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	require.NoError(t, err, "expected no error")
	require.Equal(t, res, true, "expected chain to be valid")

	// add checkpoint whitelist entry
	s.ProcessCheckpoint(uint64(2), common.Hash{})
	require.Equal(t, s.checkpoint.doExist, true, "expected true as checkpoint exists")

	// case4: correct fetchHeadersByNumber function provided with wrong header
	// for block number 2. Should consider the chain as invalid and throw an error
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	require.Equal(t, err, ErrMismatch, "expected mismatch error")
	require.Equal(t, res, false, "expected chain to be invalid")

	// create a mock function, returning a the required header
	fetchHeadersByNumber = func(number uint64, _ int, _ int, _ bool) ([]*types.Header, []common.Hash, error) {
		hash := common.Hash{}
		header := types.Header{Number: big.NewInt(0)}

		switch number {
		case 0:
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 1:
			header.Number = big.NewInt(1)
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 2:
			header.Number = big.NewInt(2)
			return []*types.Header{&header}, []common.Hash{hash}, nil

		case 3:
			header.Number = big.NewInt(3)
			hash3 := common.Hash{3}

			return []*types.Header{&header}, []common.Hash{hash3}, nil

		default:
			return nil, nil, errors.New("invalid number")
		}
	}

	s.ProcessMilestone(uint64(3), common.Hash{})

	//Case5: correct fetchHeadersByNumber function provided with hash mismatch, should consider the chain as invalid
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	require.Equal(t, err, ErrMismatch, "expected milestone mismatch error")
	require.Equal(t, res, false, "expected chain to be invalid")

	s.ProcessMilestone(uint64(2), common.Hash{})

	// create a mock function, returning a the required header
	fetchHeadersByNumber = func(number uint64, _ int, _ int, _ bool) ([]*types.Header, []common.Hash, error) {
		hash := common.Hash{}
		header := types.Header{Number: big.NewInt(0)}

		switch number {
		case 0:
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 1:
			header.Number = big.NewInt(1)
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 2:
			header.Number = big.NewInt(2)
			return []*types.Header{&header}, []common.Hash{hash}, nil
		default:
			return nil, nil, errors.New("invalid number")
		}
	}

	// case6: correct fetchHeadersByNumber function provided, should consider the chain as valid
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	require.NoError(t, err, "expected no error")
	require.Equal(t, res, true, "expected chain to be valid")

	// create a mock function, returning a the required header
	fetchHeadersByNumber = func(number uint64, _ int, _ int, _ bool) ([]*types.Header, []common.Hash, error) {
		hash := common.Hash{}
		hash3 := common.Hash{3}
		header := types.Header{Number: big.NewInt(0)}

		switch number {
		case 0:
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 1:
			header.Number = big.NewInt(1)
			return []*types.Header{&header}, []common.Hash{hash}, nil
		case 2:
			header.Number = big.NewInt(2)
			return []*types.Header{&header}, []common.Hash{hash}, nil

		case 3:
			header.Number = big.NewInt(2) // sending wrong header for misamatch
			return []*types.Header{&header}, []common.Hash{hash}, nil

		case 4:
			header.Number = big.NewInt(4) // sending wrong header for misamatch
			return []*types.Header{&header}, []common.Hash{hash3}, nil
		default:
			return nil, nil, errors.New("invalid number")
		}
	}

	//Add one more milestone in the list
	s.ProcessMilestone(uint64(3), common.Hash{})

	// case7: correct fetchHeadersByNumber function provided with wrong header for block 3, should consider the chain as invalid
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	require.Equal(t, err, ErrMismatch, "expected milestone mismatch error")
	require.Equal(t, res, false, "expected chain to be invalid")

	//require.Equal(t, s.milestone.length(), 3, "expected 3 items in milestoneList")

	//Add one more milestone in the list
	s.ProcessMilestone(uint64(4), common.Hash{})

	// case8: correct fetchHeadersByNumber function provided with wrong hash for block 3, should consider the chain as valid
	res, err = s.IsValidPeer(nil, fetchHeadersByNumber)
	require.Equal(t, err, ErrMismatch, "expected milestone mismatch error")
	require.Equal(t, res, false, "expected chain to be invalid")
}

// TestIsValidChain checks the IsValidChain function in isolation
// for different cases by providing a mock current header and chain
func TestIsValidChain(t *testing.T) {
	t.Parallel()

	s := NewMockService()
	chainA := createMockChain(1, 20) // A1->A2...A19->A20

	//Case1: no checkpoint whitelist and no milestone and no locking, should consider the chain as valid
	res := s.IsValidChain(nil, chainA)
	require.Equal(t, res, true, "Expected chain to be valid")

	tempChain := createMockChain(21, 22) // A21->A22

	// add mock checkpoint entry
	s.ProcessCheckpoint(tempChain[1].Number.Uint64(), tempChain[1].Hash())

	//Make the mock chain with zero blocks
	zeroChain := make([]*types.Header, 0)

	//Case2: As input chain is of zero length,should consider the chain as invalid
	res = s.IsValidChain(nil, zeroChain)
	require.Equal(t, res, false, "expected chain to be invalid", len(zeroChain))

	//Case3: As the received chain is behind the oldest whitelisted block entry, should consider
	// the chain as invalid as we won't require the chain.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid")

	// add mock milestone entry
	s.ProcessMilestone(tempChain[1].Number.Uint64(), tempChain[1].Hash())

	//Case4: As the received chain is behind the oldest whitelisted block entry, should consider
	// the chain as invalid as we won't require the chain.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid")

	//Remove the whitelisted checkpoint
	s.PurgeWhitelistedCheckpoint()

	//Case5: As the received chain is still invalid after removing the checkpoint as it is
	//still behind the whitelisted milestone
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid")

	//Remove the whitelisted milestone
	s.PurgeWhitelistedMilestone()

	//At this stage there is no whitelisted milestone and checkpoint

	//Locking for sprintNumber 15
	s.LockMutex(chainA[len(chainA)-5].Number.Uint64())
	s.UnlockMutex(true, "MilestoneID1", chainA[len(chainA)-5].Hash())

	//Case6: As the received chain is valid as the locked sprintHash matches with the incoming chain.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, true, "expected chain to be valid as incoming chain matches with the locked value ")

	hash3 := common.Hash{3}

	//Locking for sprintNumber 16 with differnt hash
	s.LockMutex(chainA[len(chainA)-4].Number.Uint64())
	s.UnlockMutex(true, "MilestoneID2", hash3)

	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid as incoming chain does match with the locked value hash ")

	//Locking for sprintNumber 19
	s.LockMutex(chainA[len(chainA)-1].Number.Uint64())
	s.UnlockMutex(true, "MilestoneID1", chainA[len(chainA)-1].Hash())

	//Case7: As the received chain is valid as the locked sprintHash matches with the incoming chain.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid as incoming chain is less than the locked value ")

	//Locking for sprintNumber 19
	s.LockMutex(uint64(21))
	s.UnlockMutex(true, "MilestoneID1", hash3)

	//Case8: As the received chain is invalid as the locked sprintHash matches is ahead of incoming chain.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid as incoming chain is less than the locked value ")

	//Unlocking the sprint
	s.UnlockSprint(uint64(21))

	// Clear checkpoint whitelist and add block A15 in whitelist
	s.PurgeWhitelistedCheckpoint()
	s.ProcessCheckpoint(chainA[15].Number.Uint64(), chainA[15].Hash())

	require.Equal(t, s.checkpoint.doExist, true, "expected true as checkpoint exists.")

	// case9: As the received chain is having valid checkpoint,should consider the chain as valid.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, true, "expected chain to be valid")

	// add mock milestone entries
	s.ProcessMilestone(tempChain[1].Number.Uint64(), tempChain[1].Hash())

	// case10: Try importing a past chain having valid checkpoint, should
	// consider the chain as invalid as still lastest milestone is ahead of the chain.
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid")

	// add mock milestone entries
	s.ProcessMilestone(chainA[19].Number.Uint64(), chainA[19].Hash())

	// case12: Try importing a past chain having valid checkpoint and milestone, should
	// consider the chain as valid
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, true, "expected chain to be invalid")

	// add mock milestone entries
	s.ProcessMilestone(chainA[19].Number.Uint64(), chainA[19].Hash())

	// case13: Try importing a past chain having valid checkpoint and milestone, should
	// consider the chain as valid
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, true, "expected chain to be valid")

	// add mock milestone entries with wrong hash
	s.ProcessMilestone(chainA[19].Number.Uint64(), chainA[18].Hash())

	// case14: Try importing a past chain having valid checkpoint and milestone with wrong hash, should
	// consider the chain as invalid
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid as hash mismatches")

	// Clear milestone and add blocks A15 in whitelist
	s.ProcessMilestone(chainA[15].Number.Uint64(), chainA[15].Hash())

	// case16: Try importing a past chain having valid checkpoint, should
	// consider the chain as valid
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, true, "expected chain to be valid")

	// Clear checkpoint whitelist and mock blocks in whitelist
	tempChain = createMockChain(20, 20) // A20

	s.PurgeWhitelistedCheckpoint()
	s.ProcessCheckpoint(tempChain[0].Number.Uint64(), tempChain[0].Hash())

	require.Equal(t, s.checkpoint.doExist, true, "expected true")

	// case17: Try importing a past chain having invalid checkpoint,should consider the chain as invalid
	res = s.IsValidChain(chainA[len(chainA)-1], chainA)
	require.Equal(t, res, false, "expected chain to be invalid")

	// case18: Try importing a future chain but within interval, should consider the chain as valid
	res = s.IsValidChain(tempChain[len(tempChain)-1], tempChain)
	require.Equal(t, res, true, "expected chain to be invalid")

	// create a future chain to be imported of length <= `checkpointInterval`
	chainB := createMockChain(21, 30) // B21->B22...B29->B30

	// case19: Try importing a future chain of acceptable length,should consider the chain as valid
	res = s.IsValidChain(chainA[len(chainA)-1], chainB)
	require.Equal(t, res, true, "expected chain to be valid")

	// create a future chain to be imported of length > `checkpointInterval`x
	chainB = createMockChain(21, 300) // C21->C22...C39->C40...C->256

	// case20: Try importing a future chain of unacceptable length,should consider the chain as invalid
	res = s.IsValidChain(chainA[len(chainA)-1], chainB)
	require.Equal(t, res, false, "expected chain to be invalid")
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
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			past, future := splitChain(tc.current, tc.chain)
			require.Equal(t, len(past), tc.result.pastLength)
			require.Equal(t, len(future), tc.result.futureLength)

			if len(past) > 0 {
				// Check if we have expected block/s
				require.Equal(t, past[0].Number.Uint64(), tc.result.pastStart)
				require.Equal(t, past[len(past)-1].Number.Uint64(), tc.result.pastEnd)
			}

			if len(future) > 0 {
				// Check if we have expected block/s
				require.Equal(t, future[0].Number.Uint64(), tc.result.futureStart)
				require.Equal(t, future[len(future)-1].Number.Uint64(), tc.result.futureEnd)
			}
		})
	}
}

//nolint:gocognit
func TestSplitChainProperties(t *testing.T) {
	t.Parallel()

	// Current chain is at block: X
	// Incoming chain is represented as [N, M]

	currentChain := []int{0, 1, 2, 3, 10, 100} // blocks starting from genesis
	blockDiffs := []int{0, 1, 2, 3, 4, 5, 9, 10, 11, 12, 90, 100, 101, 102}

	caseParams := make(map[int]map[int]map[int]struct{}) // X -> N -> M

	for _, current := range currentChain {
		// past cases only + past to current
		for _, diff := range blockDiffs {
			from := current - diff

			// use int type for everything to not care about underflow
			if from < 0 {
				continue
			}

			for _, diff := range blockDiffs {
				to := current - diff

				if to >= from {
					addTestCaseParams(caseParams, current, from, to)
				}
			}
		}

		// future only + current to future
		for _, diff := range blockDiffs {
			from := current + diff

			if from < 0 {
				continue
			}

			for _, diff := range blockDiffs {
				to := current + diff

				if to >= from {
					addTestCaseParams(caseParams, current, from, to)
				}
			}
		}

		// past-current-future
		for _, diff := range blockDiffs {
			from := current - diff

			if from < 0 {
				continue
			}

			for _, diff := range blockDiffs {
				to := current + diff

				if to >= from {
					addTestCaseParams(caseParams, current, from, to)
				}
			}
		}
	}

	type testCase struct {
		current     int
		remoteStart int
		remoteEnd   int
	}

	var ts []testCase

	// X -> N -> M
	for x, nm := range caseParams {
		for n, mMap := range nm {
			for m := range mMap {
				ts = append(ts, testCase{x, n, m})
			}
		}
	}

	//nolint:paralleltest
	for i, tc := range ts {
		tc := tc

		name := fmt.Sprintf("test case: index = %d, X = %d, N = %d, M = %d", i, tc.current, tc.remoteStart, tc.remoteEnd)

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			chain := createMockChain(uint64(tc.remoteStart), uint64(tc.remoteEnd))

			past, future := splitChain(uint64(tc.current), chain)

			// properties
			if len(past) > 0 {
				// Check if the chain is ordered
				isOrdered := sort.SliceIsSorted(past, func(i, j int) bool {
					return past[i].Number.Uint64() < past[j].Number.Uint64()
				})

				require.True(t, isOrdered, "an ordered past chain expected: %v", past)

				isSequential := sort.SliceIsSorted(past, func(i, j int) bool {
					return past[i].Number.Uint64() == past[j].Number.Uint64()-1
				})

				require.True(t, isSequential, "a sequential past chain expected: %v", past)

				// Check if current block >= past chain's last block
				require.Equal(t, past[len(past)-1].Number.Uint64() <= uint64(tc.current), true)
			}

			if len(future) > 0 {
				// Check if the chain is ordered
				isOrdered := sort.SliceIsSorted(future, func(i, j int) bool {
					return future[i].Number.Uint64() < future[j].Number.Uint64()
				})

				require.True(t, isOrdered, "an ordered future chain expected: %v", future)

				isSequential := sort.SliceIsSorted(future, func(i, j int) bool {
					return future[i].Number.Uint64() == future[j].Number.Uint64()-1
				})

				require.True(t, isSequential, "a sequential future chain expected: %v", future)

				// Check if future chain's first block > current block
				require.Equal(t, future[len(future)-1].Number.Uint64() > uint64(tc.current), true)
			}

			// Check if both chains are continuous
			if len(past) > 0 && len(future) > 0 {
				require.Equal(t, past[len(past)-1].Number.Uint64(), future[0].Number.Uint64()-1)
			}

			// Check if we get the original chain on appending both
			gotChain := append(past, future...)
			require.Equal(t, reflect.DeepEqual(gotChain, chain), true)
		})
	}
}

// createMockChain returns a chain with dummy headers
// starting from `start` to `end` (inclusive)
func createMockChain(start, end uint64) []*types.Header {
	var (
		i     uint64
		idx   uint64
		chain []*types.Header = make([]*types.Header, end-start+1)
	)

	for i = start; i <= end; i++ {
		header := &types.Header{
			Number: big.NewInt(int64(i)),
			Time:   uint64(time.Now().UnixMicro()) + i,
		}
		chain[idx] = header
		idx++
	}

	return chain
}

// mXNM should be initialized
func addTestCaseParams(mXNM map[int]map[int]map[int]struct{}, x, n, m int) {
	//nolint:ineffassign
	mNM, ok := mXNM[x]
	if !ok {
		mNM = make(map[int]map[int]struct{})
		mXNM[x] = mNM
	}

	//nolint:ineffassign
	_, ok = mNM[n]
	if !ok {
		mM := make(map[int]struct{})
		mNM[n] = mM
	}

	mXNM[x][n][m] = struct{}{}
}
