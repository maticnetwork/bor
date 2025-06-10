package bor

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"
	"go.uber.org/mock/gomock"
)

type MockHeimdallClient struct {
}

func (h *MockHeimdallClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	// Throw error for span id 100
	if spanID == 100 {
		return nil, fmt.Errorf("unable to fetch span")
	}

	// Create mock validators for testing
	validators := []*stakeTypes.Validator{
		{
			ValId:            1,
			Signer:           "0x96C42C56fdb78294F96B0cFa33c92bed7D75F96a",
			VotingPower:      100,
			ProposerPriority: 0,
		},
	}

	validatorSet := stakeTypes.ValidatorSet{
		Validators: validators,
		Proposer:   validators[0],
	}

	selectedProducers := []stakeTypes.Validator{
		{
			ValId:            1,
			Signer:           "0x96C42C56fdb78294F96B0cFa33c92bed7D75F96a",
			VotingPower:      100,
			ProposerPriority: 0,
		},
	}

	// For everything else, return hardcoded span assuming length 6400 (except for span 0)
	if spanID == 0 {
		return &types.Span{
			Id:                0,
			StartBlock:        0,
			EndBlock:          255,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	} else {
		return &types.Span{
			Id:                spanID,
			StartBlock:        6400*(spanID-1) + 256,
			EndBlock:          6400*spanID + 255,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	}
}

func (h *MockHeimdallClient) GetLatestSpan(ctx context.Context) (*types.Span, error) {
	return h.GetSpan(ctx, 2)
}

func TestSpanStore_SpanById(t *testing.T) {
	spanStore := NewSpanStore(&MockHeimdallClient{}, nil, "1337")
	defer spanStore.Close()
	ctx := context.Background()

	type Testcase struct {
		id         uint64
		startBlock uint64
		endBlock   uint64
	}

	testcases := []Testcase{
		{id: 0, startBlock: 0, endBlock: 255},
		{id: 1, startBlock: 256, endBlock: 6655},
		{id: 2, startBlock: 6656, endBlock: 13055},
	}

	for _, tc := range testcases {
		t.Run("", func(t *testing.T) {
			span, err := spanStore.spanById(ctx, tc.id)
			require.NoError(t, err, "err in spanById for id=%d", tc.id)
			require.Equal(t, tc.id, span.ID, "invalid id in spanById for id=%d", tc.id)
			require.Equal(t, tc.startBlock, span.StartBlock, "invalid start block in spanById for id=%d", tc.id)
			require.Equal(t, tc.endBlock, span.EndBlock, "invalid end block in spanById for id=%d", tc.id)
		})
	}

	// Ensure cache is updated
	keys := spanStore.store.Keys()
	require.Len(t, keys, 3, "invalid length of keys in span store")

	// Ask for a few more spans
	for i := uint64(0); i <= 20; i++ {
		_, err := spanStore.spanById(ctx, i)
		require.NoError(t, err, "err in spanById for id=%d", i)
	}

	// Ensure cache is updated
	keys = spanStore.store.Keys()
	require.Len(t, keys, 10, "invalid length of keys in span store")

	// Ensure we're still able to fetch old spans even though they're evicted from cache
	span, err := spanStore.spanById(ctx, 0)
	require.NoError(t, err, "err in spanById after eviction for id=0")
	require.Equal(t, uint64(0), span.ID, "invalid id in spanById after eviction for id=0")
	require.Equal(t, uint64(0), span.StartBlock, "invalid start block in spanById after eviction for id=0")
	require.Equal(t, uint64(255), span.EndBlock, "invalid end block in spanById after eviction for id=0")

	// Try fetching span 100 and ensure error is handled
	span, err = spanStore.spanById(ctx, 100)
	require.Error(t, err, "expected error in spanById for id=100")
	require.Nil(t, span, "expected nil span in spanById for id=100")
}

func TestSpanStore_SpanByBlockNumber(t *testing.T) {
	spanStore := NewSpanStore(&MockHeimdallClient{}, nil, "1337")
	defer spanStore.Close()
	ctx := context.Background()

	type Testcase struct {
		blockNumber uint64
		id          uint64
		startBlock  uint64
		endBlock    uint64
	}

	// Insert a few spans
	for i := uint64(0); i < 3; i++ {
		_, err := spanStore.spanById(ctx, i)
		require.NoError(t, err, "err in spanById for id=%d", i)
	}

	// Ensure cache is updated
	keys := spanStore.store.Keys()
	require.Len(t, keys, 3, "invalid length of keys in span store")

	// Ask for current and past spans via block number
	testcases := []Testcase{
		{blockNumber: 0, id: 0, startBlock: 0, endBlock: 255},
		{blockNumber: 1, id: 0, startBlock: 0, endBlock: 255},
		{blockNumber: 255, id: 0, startBlock: 0, endBlock: 255},
		{blockNumber: 256, id: 1, startBlock: 256, endBlock: 6655},
		{blockNumber: 257, id: 1, startBlock: 256, endBlock: 6655},
		{blockNumber: 6000, id: 1, startBlock: 256, endBlock: 6655},
		{blockNumber: 6655, id: 1, startBlock: 256, endBlock: 6655},
		{blockNumber: 6656, id: 2, startBlock: 6656, endBlock: 13055},
		{blockNumber: 10000, id: 2, startBlock: 6656, endBlock: 13055},
		{blockNumber: 13055, id: 2, startBlock: 6656, endBlock: 13055},
	}

	for _, tc := range testcases {
		t.Run("", func(t *testing.T) {
			span, err := spanStore.spanByBlockNumber(ctx, tc.blockNumber)
			require.NoError(t, err, "err in spanByBlockNumber for block=%d", tc.blockNumber)
			require.Equal(t, tc.id, span.ID, "invalid id in spanByBlockNumber for block=%d", tc.blockNumber)
			require.Equal(t, tc.startBlock, span.StartBlock, "invalid start block in spanByBlockNumber for block=%d", tc.blockNumber)
			require.Equal(t, tc.endBlock, span.EndBlock, "invalid end block in spanByBlockNumber for block=%d", tc.blockNumber)
		})
	}

	// Insert a few more spans to trigger eviction
	for i := uint64(0); i <= 20; i++ {
		_, err := spanStore.spanById(ctx, i)
		require.NoError(t, err, "err in spanById for id=%d", i)
	}

	// Ensure cache is updated
	keys = spanStore.store.Keys()
	require.Len(t, keys, 10, "invalid length of keys in span store")

	// Ask for current and past spans
	testcases = append(testcases, Testcase{blockNumber: 57856, id: 10, startBlock: 57856, endBlock: 64255})
	testcases = append(testcases, Testcase{blockNumber: 60000, id: 10, startBlock: 57856, endBlock: 64255})
	testcases = append(testcases, Testcase{blockNumber: 64255, id: 10, startBlock: 57856, endBlock: 64255})
	testcases = append(testcases, Testcase{blockNumber: 121856, id: 20, startBlock: 121856, endBlock: 128255})
	testcases = append(testcases, Testcase{blockNumber: 122000, id: 20, startBlock: 121856, endBlock: 128255})
	testcases = append(testcases, Testcase{blockNumber: 128255, id: 20, startBlock: 121856, endBlock: 128255})

	for _, tc := range testcases {
		t.Run("", func(t *testing.T) {
			span, err := spanStore.spanByBlockNumber(ctx, tc.blockNumber)
			require.NoError(t, err, "err in spanByBlockNumber for block=%d", tc.blockNumber)
			require.Equal(t, tc.id, span.ID, "invalid id in spanByBlockNumber for block=%d", tc.blockNumber)
			require.Equal(t, tc.startBlock, span.StartBlock, "invalid start block in spanByBlockNumber for block=%d", tc.blockNumber)
			require.Equal(t, tc.endBlock, span.EndBlock, "invalid end block in spanByBlockNumber for block=%d", tc.blockNumber)
		})
	}

	// Asking for a future span
	span, err := spanStore.spanByBlockNumber(ctx, 128256) // block 128256 belongs to span 21 (future span)
	require.NoError(t, err, "err in spanByBlockNumber for future block 128256")
	require.Equal(t, uint64(21), span.ID, "invalid id in spanByBlockNumber for future block 128256")
	require.Equal(t, uint64(128256), span.StartBlock, "invalid start block in spanByBlockNumber for future block 128256")
	require.Equal(t, uint64(134655), span.EndBlock, "invalid end block in spanByBlockNumber for future block 128256")
}

// Irrelevant to the tests above but necessary for interface compatibility
func (h *MockHeimdallClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchMilestone(ctx context.Context) (*milestone.Milestone, error) {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchNoAckMilestone(ctx context.Context, milestoneID string) error {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchLastNoAckMilestone(ctx context.Context) (string, error) {
	panic("implement me")
}
func (h *MockHeimdallClient) FetchMilestoneID(ctx context.Context, milestoneID string) error {
	panic("implement me")
}
func (h *MockHeimdallClient) Close() {
	panic("implement me")
}

// MockOverlappingHeimdallClient simulates overlapping spans for testing
type MockOverlappingHeimdallClient struct {
	failGetLatestSpan bool
	latestSpanID      *uint64
}

func (h *MockOverlappingHeimdallClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	// Create mock validators for testing
	validators := []*stakeTypes.Validator{
		{
			ValId:            1,
			Signer:           "0x96C42C56fdb78294F96B0cFa33c92bed7D75F96a",
			VotingPower:      100,
			ProposerPriority: 0,
		},
	}

	validatorSet := stakeTypes.ValidatorSet{
		Validators: validators,
		Proposer:   validators[0],
	}

	selectedProducers := []stakeTypes.Validator{
		{
			ValId:            1,
			Signer:           "0x96C42C56fdb78294F96B0cFa33c92bed7D75F96a",
			VotingPower:      100,
			ProposerPriority: 0,
		},
	}

	// Create overlapping spans for testing:
	// Span 0: blocks 0-99
	// Span 1: blocks 100-199
	// Span 2: blocks 200-299
	// Span 3: blocks 150-249 (overlaps with spans 1 and 2)
	// Span 4: blocks 250-349 (overlaps with spans 2 and 3)
	// Span 5: blocks 175-225 (overlaps with spans 1, 2, and 3)
	switch spanID {
	case 0:
		return &types.Span{
			Id:                0,
			StartBlock:        0,
			EndBlock:          99,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 1:
		return &types.Span{
			Id:                1,
			StartBlock:        100,
			EndBlock:          199,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 2:
		return &types.Span{
			Id:                2,
			StartBlock:        200,
			EndBlock:          299,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 3:
		return &types.Span{
			Id:                3,
			StartBlock:        150,
			EndBlock:          249,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 4:
		return &types.Span{
			Id:                4,
			StartBlock:        250,
			EndBlock:          349,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 5:
		return &types.Span{
			Id:                5,
			StartBlock:        175,
			EndBlock:          225,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 6:
		return &types.Span{
			Id:                6,
			StartBlock:        400,
			EndBlock:          499,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	case 8:
		return &types.Span{
			Id:                8,
			StartBlock:        600,
			EndBlock:          699,
			ValidatorSet:      validatorSet,
			SelectedProducers: selectedProducers,
		}, nil
	default:
		return nil, fmt.Errorf("span %d not found", spanID)
	}
}

func (h *MockOverlappingHeimdallClient) GetLatestSpan(ctx context.Context) (*types.Span, error) {
	if h.failGetLatestSpan {
		return nil, fmt.Errorf("latest span fetch error")
	}
	var spanID uint64 = 6 // default
	if h.latestSpanID != nil {
		spanID = *h.latestSpanID
	}
	return h.GetSpan(ctx, spanID)
}

// Implement interface methods for MockOverlappingHeimdallClient
func (h *MockOverlappingHeimdallClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchMilestone(ctx context.Context) (*milestone.Milestone, error) {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchNoAckMilestone(ctx context.Context, milestoneID string) error {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchLastNoAckMilestone(ctx context.Context) (string, error) {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) FetchMilestoneID(ctx context.Context, milestoneID string) error {
	panic("implement me")
}
func (h *MockOverlappingHeimdallClient) Close() {
	panic("implement me")
}

func TestSpanStore_SpanByBlockNumber_OverlappingSpans(t *testing.T) {
	spanStore := NewSpanStore(&MockOverlappingHeimdallClient{}, nil, "1337")
	defer spanStore.Close()
	ctx := context.Background()

	// Pre-load spans 0-4 into cache to simulate known spans
	for i := uint64(0); i <= 4; i++ {
		_, err := spanStore.spanById(ctx, i)
		require.NoError(t, err, "err in spanById for id=%d", i)
	}

	// Test cases for overlapping spans
	// Expected behavior: always return span with highest ID that contains the block
	type Testcase struct {
		name        string
		blockNumber uint64
		expectedID  uint64
		description string
	}

	testcases := []Testcase{
		// Non-overlapping blocks
		{
			name:        "block_in_span_0_only",
			blockNumber: 50,
			expectedID:  0,
			description: "Block 50 is only in span 0 (0-99)",
		},
		{
			name:        "block_in_span_1_only",
			blockNumber: 120,
			expectedID:  1,
			description: "Block 120 is only in span 1 (100-199)",
		},

		// Overlapping blocks - should return highest span ID
		{
			name:        "block_overlapping_spans_1_and_3",
			blockNumber: 175,
			expectedID:  5,
			description: "Block 175 is in spans 1 (100-199), 3 (150-249), and 5 (175-225), should return span 5",
		},
		{
			name:        "block_overlapping_spans_2_and_3",
			blockNumber: 225,
			expectedID:  5,
			description: "Block 225 is in spans 2 (200-299), 3 (150-249), and 5 (175-225), should return span 5",
		},
		{
			name:        "block_overlapping_spans_2_and_4",
			blockNumber: 275,
			expectedID:  4,
			description: "Block 275 is in spans 2 (200-299) and 4 (250-349), should return span 4",
		},

		// Edge cases at boundaries
		{
			name:        "block_at_span_boundary_start",
			blockNumber: 150,
			expectedID:  3,
			description: "Block 150 is at start of span 3 and also in span 1, should return span 3",
		},
		{
			name:        "block_at_span_boundary_end",
			blockNumber: 199,
			expectedID:  5,
			description: "Block 199 is at end of span 1 and also in span 3 and 5, should return span 5",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			span, err := spanStore.spanByBlockNumber(ctx, tc.blockNumber)
			require.NoError(t, err, "err in spanByBlockNumber for block=%d: %s", tc.blockNumber, tc.description)
			require.Equal(t, tc.expectedID, span.ID, "invalid span ID for block=%d: %s", tc.blockNumber, tc.description)
		})
	}
}

func TestSpanStore_SpanByBlockNumber_OverlappingSpansWithFuture(t *testing.T) {
	spanStore := NewSpanStore(&MockOverlappingHeimdallClient{}, nil, "1337")
	defer spanStore.Close()
	ctx := context.Background()

	// Pre-load spans 0-2 into cache, leaving spans 3-6 as "future" spans
	for i := uint64(0); i <= 2; i++ {
		_, err := spanStore.spanById(ctx, i)
		require.NoError(t, err, "err in spanById for id=%d", i)
	}

	// Test case where a block is in known spans but a future span has higher ID
	// Block 175 is in span 1 (known) but also in span 5 (future) which has higher ID
	span, err := spanStore.spanByBlockNumber(ctx, 175)
	require.NoError(t, err, "err in spanByBlockNumber for block 175")
	// Should return span 5 since it has higher ID than span 1
	require.Equal(t, uint64(5), span.ID, "should return span 5 for block 175 (higher ID than span 1)")

	// Test purely future span
	span, err = spanStore.spanByBlockNumber(ctx, 450)
	require.NoError(t, err, "err in spanByBlockNumber for future block 450")
	require.Equal(t, uint64(6), span.ID, "should return span 6 for block 450")
}

func TestSpanStore_SpanByBlockNumber_OverlappingSpansMultipleMatches(t *testing.T) {
	spanStore := NewSpanStore(&MockOverlappingHeimdallClient{}, nil, "1337")
	defer spanStore.Close()
	ctx := context.Background()

	// Pre-load all spans 0-6 into cache
	for i := uint64(0); i <= 6; i++ {
		_, err := spanStore.spanById(ctx, i)
		require.NoError(t, err, "err in spanById for id=%d", i)
	}

	// Test block that appears in multiple spans - should always return highest ID
	// Block 200 appears in spans 2 (200-299) and 3 (150-249)
	span, err := spanStore.spanByBlockNumber(ctx, 200)
	require.NoError(t, err, "err in spanByBlockNumber for block 200")
	require.Equal(t, uint64(5), span.ID, "should return span 5 (highest ID) for block 200")

	// Block 225 appears in spans 2 (200-299), 3 (150-249), and 5 (175-225)
	span, err = spanStore.spanByBlockNumber(ctx, 225)
	require.NoError(t, err, "err in spanByBlockNumber for block 225")
	require.Equal(t, uint64(5), span.ID, "should return span 5 (highest ID) for block 225")

	// Block 190 appears in spans 1 (100-199), 3 (150-249), and 5 (175-225)
	span, err = spanStore.spanByBlockNumber(ctx, 190)
	require.NoError(t, err, "err in spanByBlockNumber for block 190")
	require.Equal(t, uint64(5), span.ID, "should return span 5 (highest ID) for block 190")
}

func TestSpanStore_GetFutureSpan(t *testing.T) {
	ctx := context.Background()

	type testCase struct {
		name              string
		startID           uint64
		blockNumber       uint64
		latestKnownSpanID uint64 // informational
		mockClientSetup   func() IHeimdallClient
		expectedSpanID    uint64
		expectError       bool
		expectedErrorMsg  string
	}

	uintPtr := func(i uint64) *uint64 { return &i }

	testCases := []testCase{
		{
			name:              "simple future span found",
			startID:           3,
			blockNumber:       275, // in span 4 (250-349)
			latestKnownSpanID: 2,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{} // latest is 6
			},
			expectedSpanID: 4,
		},
		{
			name:              "overlapping future span, highest ID chosen",
			startID:           1,
			blockNumber:       180, // in spans 1(100-199), 3(150-249), 5(175-225)
			latestKnownSpanID: 0,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{}
			},
			expectedSpanID: 5, // should be 5 because it has the highest ID
		},
		{
			name:              "span not found",
			startID:           1,
			blockNumber:       375, // between spans 4 and 6
			latestKnownSpanID: 0,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{}
			},
			expectError:      true,
			expectedErrorMsg: "span not found for block 375",
		},
		{
			name:              "stop search after two skipped spans",
			startID:           2,
			blockNumber:       120, // not in any span starting from 2, so should fail
			latestKnownSpanID: 1,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{}
			},
			expectError:      true, // no candidate found
			expectedErrorMsg: "span not found for block 120",
		},
		{
			name:              "stop search after two skipped spans with candidate",
			startID:           1,
			blockNumber:       160, // in span 1 and 3. search should stop after checking span 5 and return span 3
			latestKnownSpanID: 0,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{}
			},
			expectedSpanID: 3,
		},
		{
			name:              "span found before an erroring span",
			startID:           6,
			blockNumber:       450, // in span 6
			latestKnownSpanID: 5,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{} // GetSpan(7) will fail by default, but we should return candidate
			},
			expectedSpanID: 6,
		},
		{
			name:              "error from spanById with no candidate",
			startID:           7, // this will fail in the mock
			blockNumber:       500,
			latestKnownSpanID: 6,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{latestSpanID: uintPtr(8)}
			},
			expectError:      true,
			expectedErrorMsg: "span 7 not found",
		},
		{
			name:              "latest span fetch fails",
			startID:           1,
			blockNumber:       200,
			latestKnownSpanID: 0,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{failGetLatestSpan: true}
			},
			expectError:      true,
			expectedErrorMsg: "latest span fetch error",
		},
		{
			name:              "id greater than latestSpan.ID at start",
			startID:           7,
			blockNumber:       200,
			latestKnownSpanID: 6,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{} // latest span is 6
			},
			expectError:      true,
			expectedErrorMsg: "span not found for block 200",
		},
		{
			name:              "block number found in first future span",
			startID:           5,
			blockNumber:       180, // in span 5
			latestKnownSpanID: 4,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{}
			},
			expectedSpanID: 5,
		},
		{
			name:              "latest span is 0",
			startID:           1,
			blockNumber:       50,
			latestKnownSpanID: 0,
			mockClientSetup: func() IHeimdallClient {
				return &MockOverlappingHeimdallClient{latestSpanID: uintPtr(0)}
			},
			expectError:      true,
			expectedErrorMsg: "span not found for block 50",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockClient := tc.mockClientSetup()
			spanStore := NewSpanStore(mockClient, nil, "1337")
			defer spanStore.Close()

			// Pre-load some spans to make the store behave more realistically
			for i := uint64(0); i <= tc.latestKnownSpanID; i++ {
				_, err := spanStore.spanById(context.Background(), i)
				require.NoError(t, err)
			}

			span, err := spanStore.getFutureSpan(ctx, tc.startID, tc.blockNumber, tc.latestKnownSpanID)

			if tc.expectError {
				require.Error(t, err)
				if tc.expectedErrorMsg != "" {
					require.Contains(t, err.Error(), tc.expectedErrorMsg)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, span)
				require.Equal(t, tc.expectedSpanID, span.ID)
			}
		})
	}
}

func TestSpanStore_EstimateSpanId(t *testing.T) {
	spanStore := NewSpanStore(nil, nil, "1337") // Heimdall client and spanner not needed for this test
	defer spanStore.Close()

	// Mock lastUsedSpan for some test cases
	mockLastUsedSpan := func(id, startBlock, endBlock uint64) {
		spanStore.lastUsedSpan.Store(&span.HeimdallSpan{
			Span: span.Span{
				ID:         id,
				StartBlock: startBlock,
				EndBlock:   endBlock,
			},
		})
	}

	testcases := []struct {
		name              string
		blockNumber       uint64
		setupLastUsedSpan func() // Function to set up lastUsedSpan if needed
		expectedSpanId    uint64
	}{
		{
			name:           "block_in_zeroth_span",
			blockNumber:    100,
			expectedSpanId: 0,
		},
		{
			name:           "block_at_zeroth_span_end",
			blockNumber:    zerothSpanEnd,
			expectedSpanId: 0,
		},
		{
			name:           "block_after_zeroth_span_no_last_used",
			blockNumber:    zerothSpanEnd + 1,
			expectedSpanId: 1,
		},
		{
			name:           "block_far_after_zeroth_span_no_last_used",
			blockNumber:    zerothSpanEnd + defaultSpanLength,
			expectedSpanId: 1 + (defaultSpanLength-1)/defaultSpanLength,
		},
		{
			name:        "block_within_last_used_span",
			blockNumber: 300,
			setupLastUsedSpan: func() {
				mockLastUsedSpan(1, zerothSpanEnd+1, zerothSpanEnd+defaultSpanLength)
			},
			expectedSpanId: 1,
		},
		{
			name:        "block_after_last_used_span",
			blockNumber: zerothSpanEnd + defaultSpanLength + 100,
			setupLastUsedSpan: func() {
				mockLastUsedSpan(1, zerothSpanEnd+1, zerothSpanEnd+defaultSpanLength)
			},
			expectedSpanId: 2,
		},
		{
			name:        "block_much_after_last_used_span",
			blockNumber: zerothSpanEnd + 3*defaultSpanLength,
			setupLastUsedSpan: func() {
				mockLastUsedSpan(1, zerothSpanEnd+1, zerothSpanEnd+defaultSpanLength)
			},
			expectedSpanId: 3,
		},
		{
			name:        "block_before_last_used_span",
			blockNumber: zerothSpanEnd + defaultSpanLength, // This block is for span 1
			setupLastUsedSpan: func() { // last used is span 5
				mockLastUsedSpan(5, zerothSpanEnd+4*defaultSpanLength+1, zerothSpanEnd+5*defaultSpanLength)
			},
			expectedSpanId: 1,
		},
		{
			name:        "block_much_before_last_used_span",
			blockNumber: zerothSpanEnd + 1, // This block is for span 1
			setupLastUsedSpan: func() { // last used is span 10
				mockLastUsedSpan(10, zerothSpanEnd+9*defaultSpanLength+1, zerothSpanEnd+10*defaultSpanLength)
			},
			expectedSpanId: 1,
		},
		{
			name:           "block_very_large_no_last_used",
			blockNumber:    1000000,
			expectedSpanId: 1 + (1000000-zerothSpanEnd-1)/defaultSpanLength,
		},
		{
			name:        "block_at_start_of_last_used_span",
			blockNumber: zerothSpanEnd + defaultSpanLength + 1,
			setupLastUsedSpan: func() {
				mockLastUsedSpan(2, zerothSpanEnd+defaultSpanLength+1, zerothSpanEnd+2*defaultSpanLength)
			},
			expectedSpanId: 2,
		},
		{
			name:        "block_at_end_of_last_used_span",
			blockNumber: zerothSpanEnd + 2*defaultSpanLength,
			setupLastUsedSpan: func() {
				mockLastUsedSpan(2, zerothSpanEnd+defaultSpanLength+1, zerothSpanEnd+2*defaultSpanLength)
			},
			expectedSpanId: 2,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset lastUsedSpan for each test case if setupLastUsedSpan is not defined
			if tc.setupLastUsedSpan == nil {
				spanStore.lastUsedSpan = atomic.Pointer[span.HeimdallSpan]{}
			} else {
				tc.setupLastUsedSpan()
			}
			estimatedId := spanStore.estimateSpanId(tc.blockNumber)
			require.Equal(t, tc.expectedSpanId, estimatedId, "Block %d", tc.blockNumber)
		})
	}
}

func TestGetMockSpan0(t *testing.T) {
	ctx := context.Background()
	chainId := "1337"
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("successful_mock_span_0", func(t *testing.T) {
		mockSpanner := NewMockSpanner(ctrl)
		mockValidators := []*valset.Validator{
			{ID: 1, Address: common.HexToAddress("0x1"), VotingPower: 100},
			{ID: 2, Address: common.HexToAddress("0x2"), VotingPower: 200},
		}
		mockSpanner.EXPECT().GetCurrentValidatorsByBlockNrOrHash(ctx, rpc.BlockNumberOrHashWithNumber(0), uint64(0)).Return(mockValidators, nil)

		s, err := getMockSpan0(ctx, mockSpanner, chainId)
		require.NoError(t, err)
		require.NotNil(t, s)
		require.Equal(t, uint64(0), s.ID)
		require.Equal(t, uint64(0), s.StartBlock)
		require.Equal(t, uint64(zerothSpanEnd), s.EndBlock) // Uses zerothSpanEnd from existing constants
		require.Equal(t, chainId, s.ChainID)
		require.Len(t, s.ValidatorSet.Validators, len(mockValidators))
		require.Len(t, s.SelectedProducers, len(mockValidators))
		for i, v := range mockValidators {
			require.Equal(t, *v, s.SelectedProducers[i])
			require.Equal(t, v, s.ValidatorSet.Validators[i])
		}
		require.Equal(t, mockValidators[0], s.ValidatorSet.Proposer)
	})

	t.Run("spanner_returns_error_from_heimdall", func(t *testing.T) {
		mockSpanner := NewMockSpanner(ctrl)
		expectedErr := fmt.Errorf("spanner error from heimdall")
		mockSpanner.EXPECT().GetCurrentValidatorsByBlockNrOrHash(ctx, rpc.BlockNumberOrHashWithNumber(0), uint64(0)).Return(nil, expectedErr)

		_, err := getMockSpan0(ctx, mockSpanner, chainId)
		require.Error(t, err)
		require.Equal(t, expectedErr, err)
	})

	t.Run("spanner_is_nil_passed_to_func", func(t *testing.T) {
		// This test case doesn't involve the mock object itself, but tests the nil check in getMockSpan0
		_, err := getMockSpan0(ctx, nil, chainId)
		require.Error(t, err)
		require.Contains(t, err.Error(), "spanner not available")
	})

	t.Run("spanner_returns_empty_validators", func(t *testing.T) {
		mockSpanner := NewMockSpanner(ctrl)
		mockSpanner.EXPECT().GetCurrentValidatorsByBlockNrOrHash(ctx, rpc.BlockNumberOrHashWithNumber(0), uint64(0)).Return([]*valset.Validator{}, nil)

		_, err := getMockSpan0(ctx, mockSpanner, chainId)
		require.Error(t, err)
		require.EqualError(t, err, "no validators found for genesis, cannot create mock span 0")
	})
}

func TestSpanStore_SetHeimdallClient(t *testing.T) {
	spanStore := NewSpanStore(nil, nil, "1337")
	defer spanStore.Close()

	require.Nil(t, spanStore.heimdallClient, "Initial heimdall client should be nil")

	mockClient := &MockHeimdallClient{}
	spanStore.setHeimdallClient(mockClient)

	require.Equal(t, mockClient, spanStore.heimdallClient, "Heimdall client not set correctly")
}

func TestSpanStore_Close(t *testing.T) {
	spanStore := NewSpanStore(&MockHeimdallClient{}, nil, "1337")
	require.NotNil(t, spanStore.cancel, "Cancel function should be set by NewSpanStore")

	spanStore.Close()
	spanStore.cancel = nil
	spanStore.Close() // Should not panic
}

// dynamicHeimdallClient is a mock IHeimdallClient for testing waitForNewSpan.
// It allows dynamically changing the spans returned during a test.
type dynamicHeimdallClient struct {
	mu         sync.Mutex
	spans      map[uint64]*types.Span
	latestSpan *types.Span
	fetchErr   error
}

func newDynamicHeimdallClient() *dynamicHeimdallClient {
	return &dynamicHeimdallClient{spans: make(map[uint64]*types.Span)}
}

func (d *dynamicHeimdallClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.fetchErr != nil {
		return nil, d.fetchErr
	}
	if s, ok := d.spans[spanID]; ok {
		// Return a copy to avoid data races if the test modifies it while it's being used.
		spanCopy := *s
		producersCopy := make([]stakeTypes.Validator, len(s.SelectedProducers))
		copy(producersCopy, s.SelectedProducers)
		spanCopy.SelectedProducers = producersCopy
		return &spanCopy, nil
	}
	return nil, fmt.Errorf("span %d not found", spanID)
}

func (d *dynamicHeimdallClient) GetLatestSpan(ctx context.Context) (*types.Span, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.fetchErr != nil {
		return nil, d.fetchErr
	}
	if d.latestSpan == nil {
		// To avoid panic if latest span is not set, which can happen in some test setups.
		if len(d.spans) > 0 {
			var maxID uint64
			for id := range d.spans {
				if id > maxID {
					maxID = id
				}
			}
			return d.spans[maxID], nil
		}
		return nil, fmt.Errorf("latest span not set in mock")
	}
	return d.latestSpan, nil
}

func (d *dynamicHeimdallClient) setSpans(spans map[uint64]*types.Span, latestSpan *types.Span) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.spans = spans
	d.latestSpan = latestSpan
}

func (d *dynamicHeimdallClient) setError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fetchErr = err
}

func (d *dynamicHeimdallClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchMilestone(ctx context.Context) (*milestone.Milestone, error) {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchNoAckMilestone(ctx context.Context, milestoneID string) error {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchLastNoAckMilestone(ctx context.Context) (string, error) {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) FetchMilestoneID(ctx context.Context, milestoneID string) error {
	panic("not implemented")
}
func (d *dynamicHeimdallClient) Close() {}

func makeTestSpan(id, start, end uint64, producerAddr common.Address) *types.Span {
	producer := stakeTypes.Validator{
		ValId:            id,
		Signer:           producerAddr.Hex(),
		VotingPower:      100,
		ProposerPriority: 0,
	}
	return &types.Span{
		Id:         id,
		StartBlock: start,
		EndBlock:   end,
		ValidatorSet: stakeTypes.ValidatorSet{
			Validators: []*stakeTypes.Validator{&producer},
			Proposer:   &producer,
		},
		SelectedProducers: []stakeTypes.Validator{producer},
	}
}

func TestSpanStore_WaitForNewSpan(t *testing.T) {
	author1 := common.HexToAddress("0xaaaa")
	author2 := common.HexToAddress("0xbbbb")
	span0 := makeTestSpan(0, 0, 99, author1)

	t.Run("success on first try", func(t *testing.T) {
		client := newDynamicHeimdallClient()
		span1 := makeTestSpan(1, 100, 200, author2)
		client.setSpans(map[uint64]*types.Span{0: span0, 1: span1}, span1)

		store := NewSpanStore(client, nil, "1337")
		defer store.Close()

		found, err := store.waitForNewSpan(150, author1, 1*time.Second)
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("success after update", func(t *testing.T) {
		client := newDynamicHeimdallClient()
		span1 := makeTestSpan(1, 100, 200, author1)
		client.setSpans(map[uint64]*types.Span{0: span0, 1: span1}, span1)

		store := NewSpanStore(client, nil, "1337")
		defer store.Close()

		go func() {
			time.Sleep(100 * time.Millisecond) // Give waitForNewSpan time to start polling
			span2 := makeTestSpan(2, 150, 250, author2)
			client.setSpans(map[uint64]*types.Span{0: span0, 1: span1, 2: span2}, span2)
		}()

		found, err := store.waitForNewSpan(150, author1, 1*time.Second)
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("timeout when producer does not change", func(t *testing.T) {
		client := newDynamicHeimdallClient()
		span1 := makeTestSpan(1, 100, 200, author1)
		client.setSpans(map[uint64]*types.Span{0: span0, 1: span1}, span1)

		store := NewSpanStore(client, nil, "1337")
		defer store.Close()

		// Use a short timeout to ensure the test doesn't run for too long
		found, err := store.waitForNewSpan(150, author1, 250*time.Millisecond)
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("error from heimdall", func(t *testing.T) {
		client := newDynamicHeimdallClient()
		expectedErr := fmt.Errorf("heimdall connection error")
		client.setError(expectedErr)

		store := NewSpanStore(client, nil, "1337")
		defer store.Close()

		found, err := store.waitForNewSpan(150, author1, 1*time.Second)
		require.Error(t, err)
		require.Contains(t, err.Error(), expectedErr.Error())
		require.False(t, found)
	})

	t.Run("empty producers list initially", func(t *testing.T) {
		client := newDynamicHeimdallClient()
		span1 := makeTestSpan(1, 100, 200, author1)
		span1.SelectedProducers = []stakeTypes.Validator{} // Empty producer list
		client.setSpans(map[uint64]*types.Span{0: span0, 1: span1}, span1)

		store := NewSpanStore(client, nil, "1337")
		defer store.Close()

		go func() {
			time.Sleep(100 * time.Millisecond)
			span2 := makeTestSpan(2, 150, 250, author2)
			client.setSpans(map[uint64]*types.Span{0: span0, 1: span1, 2: span2}, span2)
		}()

		found, err := store.waitForNewSpan(150, author1, 1*time.Second)
		require.NoError(t, err)
		require.True(t, found)
	})

	t.Run("target block not initially in span", func(t *testing.T) {
		client := newDynamicHeimdallClient()
		span1 := makeTestSpan(1, 100, 200, author1) // block 150 is now in this span
		client.setSpans(map[uint64]*types.Span{0: span0, 1: span1}, span1)

		store := NewSpanStore(client, nil, "1337")
		defer store.Close()

		go func() {
			time.Sleep(100 * time.Millisecond)
			span2 := makeTestSpan(2, 150, 250, author2) // New span that contains the block
			client.setSpans(map[uint64]*types.Span{0: span0, 1: span1, 2: span2}, span2)
		}()

		found, err := store.waitForNewSpan(150, author1, 1*time.Second)
		require.NoError(t, err)
		require.True(t, found)
	})
}
