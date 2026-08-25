package heimdall

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
)

// mockHeimdallClient is a configurable mock implementing the Endpoint interface.
type mockHeimdallClient struct {
	getSpanFn            func(ctx context.Context, spanID uint64) (*types.Span, error)
	getLatestSpanFn      func(ctx context.Context) (*types.Span, error)
	stateSyncEventsFn    func(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error)
	fetchCheckpointFn    func(ctx context.Context, number int64) (*checkpoint.Checkpoint, error)
	fetchCheckpointCntFn func(ctx context.Context) (int64, error)
	fetchMilestoneFn     func(ctx context.Context) (*milestone.Milestone, error)
	fetchMilestoneCntFn  func(ctx context.Context) (int64, error)
	fetchStatusFn        func(ctx context.Context) (*ctypes.SyncInfo, error)
	closeFn              func()
	getSpanHits          atomic.Int32
}

func (m *mockHeimdallClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	if m.stateSyncEventsFn != nil {
		return m.stateSyncEventsFn(ctx, fromID, to)
	}

	return []*clerk.EventRecordWithTime{}, nil
}

func (m *mockHeimdallClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	m.getSpanHits.Add(1)

	if m.getSpanFn != nil {
		return m.getSpanFn(ctx, spanID)
	}

	return &types.Span{Id: spanID}, nil
}

func (m *mockHeimdallClient) GetLatestSpan(ctx context.Context) (*types.Span, error) {
	if m.getLatestSpanFn != nil {
		return m.getLatestSpanFn(ctx)
	}

	return &types.Span{Id: 99}, nil
}

func (m *mockHeimdallClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	if m.fetchCheckpointFn != nil {
		return m.fetchCheckpointFn(ctx, number)
	}

	return &checkpoint.Checkpoint{}, nil
}

func (m *mockHeimdallClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	if m.fetchCheckpointCntFn != nil {
		return m.fetchCheckpointCntFn(ctx)
	}

	return 10, nil
}

func (m *mockHeimdallClient) FetchMilestone(ctx context.Context) (*milestone.Milestone, error) {
	if m.fetchMilestoneFn != nil {
		return m.fetchMilestoneFn(ctx)
	}

	return &milestone.Milestone{}, nil
}

func (m *mockHeimdallClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	if m.fetchMilestoneCntFn != nil {
		return m.fetchMilestoneCntFn(ctx)
	}

	return 5, nil
}

func (m *mockHeimdallClient) FetchStatus(ctx context.Context) (*ctypes.SyncInfo, error) {
	if m.fetchStatusFn != nil {
		return m.fetchStatusFn(ctx)
	}

	return &ctypes.SyncInfo{}, nil
}

func (m *mockHeimdallClient) Close() {
	if m.closeFn != nil {
		m.closeFn()
	}
}

// testConnErr is a reusable connection-refused error for tests.
var testConnErr = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

// newConnRefusedMock creates a mock where both API calls and health probes always fail.
func newConnRefusedMock() *mockHeimdallClient {
	return &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, testConnErr
		},
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			return nil, testConnErr
		},
	}
}

// newToggleMock creates a mock whose API calls and health probes fail when down.Load() is true.
func newToggleMock(down *atomic.Bool) *mockHeimdallClient {
	return &mockHeimdallClient{
		getSpanFn: func(_ context.Context, spanID uint64) (*types.Span, error) {
			if down.Load() {
				return nil, testConnErr
			}
			return &types.Span{Id: spanID}, nil
		},
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			if down.Load() {
				return nil, testConnErr
			}
			return &ctypes.SyncInfo{}, nil
		},
	}
}

// newProbeToggleMock creates a mock where API calls always fail but health probes
// succeed when down.Load() is false.
func newProbeToggleMock(down *atomic.Bool) *mockHeimdallClient {
	return &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, testConnErr
		},
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			if down.Load() {
				return nil, testConnErr
			}
			return &ctypes.SyncInfo{}, nil
		},
	}
}

// newInstantMulti creates a MultiHeimdallClient with instant health registry
// behavior: consecutiveThreshold=1, promotionCooldown=0, fast health-check interval.
func newInstantMulti(clients ...Endpoint) *MultiHeimdallClient {
	fc, err := NewMultiHeimdallClient(clients...)
	if err != nil {
		panic(err)
	}

	fc.attemptTimeout = 100 * time.Millisecond
	fc.probeTimeout = 100 * time.Millisecond
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	fc.registry.HealthCheckInterval = 50 * time.Millisecond

	return fc
}

func TestNewMultiHeimdallClient_NoClients_ReturnsError(t *testing.T) {
	_, err := NewMultiHeimdallClient()
	require.Error(t, err)
}

func TestFailover_SwitchOnPrimaryDown(t *testing.T) {
	switchesBefore := failoverSwitchCounter.Snapshot().Count()
	activeBefore := failoverActiveGauge.Snapshot().Value()

	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	// Primary must fail both GetSpan and FetchStatus so the background probe
	// cannot promote it back to healthy and flip the active gauge back to 0
	// before this test's final assertion.
	primary := &mockHeimdallClient{
		getSpanFn:     func(ctx context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) { return nil, connErr },
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	span, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, span)

	assert.GreaterOrEqual(t, primary.getSpanHits.Load(), int32(1), "primary should have been tried")
	assert.GreaterOrEqual(t, secondary.getSpanHits.Load(), int32(1), "secondary should have been called")

	assert.Greater(t, failoverSwitchCounter.Snapshot().Count(), switchesBefore, "failover switch counter should increment")
	_ = activeBefore // gauge is set, not incremented
	assert.Equal(t, int64(1), failoverActiveGauge.Snapshot().Value(), "active gauge should reflect secondary index")
}

func TestFailover_NoSwitchOnContextCanceled(t *testing.T) {
	primary := &mockHeimdallClient{
		getSpanFn: func(ctx context.Context, _ uint64) (*types.Span, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 5 * time.Second // longer than caller's ctx
	fc.probeTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	secondaryBefore := secondary.getSpanHits.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = fc.GetSpan(ctx, 1)
	require.Error(t, err)
	assert.Equal(t, secondaryBefore, secondary.getSpanHits.Load(), "should not failover on caller context cancellation")
}

func TestFailover_NoSwitchOnServiceUnavailable(t *testing.T) {
	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, ErrServiceUnavailable
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	_, err = fc.GetSpan(context.Background(), 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrServiceUnavailable))
	assert.Equal(t, int32(0), secondary.getSpanHits.Load(), "should not failover on 503")
}

func TestFailover_NoSwitchOnShutdownDetected(t *testing.T) {
	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, ErrShutdownDetected
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	_, err = fc.GetSpan(context.Background(), 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrShutdownDetected))
	assert.Equal(t, int32(0), secondary.getSpanHits.Load(), "should not failover on shutdown")
}

func TestFailover_StickyBehavior(t *testing.T) {
	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.probeTimeout = 100 * time.Millisecond
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	fc.registry.HealthCheckInterval = 1 * time.Hour // very long — no background promotion
	defer fc.Close()

	// First call triggers failover
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	primaryBefore := primary.getSpanHits.Load()
	secondaryBefore := secondary.getSpanHits.Load()

	// Subsequent calls should go directly to secondary without trying primary
	for i := 0; i < 3; i++ {
		_, err = fc.GetSpan(context.Background(), 1)
		require.NoError(t, err)
	}

	assert.Equal(t, primaryBefore, primary.getSpanHits.Load(), "primary should not be contacted while sticky")
	assert.Equal(t, secondaryBefore+3, secondary.getSpanHits.Load(), "all calls should go to secondary")
}

func TestFailover_ProbeBackToPrimary(t *testing.T) {
	primaryDown := atomic.Bool{}
	primaryDown.Store(true)

	primary := newToggleMock(&primaryDown)
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	// Trigger failover
	_, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Bring primary back
	primaryDown.Store(false)

	// Wait for background health registry to promote primary
	require.Eventually(t, func() bool {
		return fc.registry.Active() == 0
	}, 2*time.Second, 20*time.Millisecond, "health registry should promote back to primary")

	// Verify subsequent calls go to primary
	secondaryBefore := secondary.getSpanHits.Load()
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, secondaryBefore, secondary.getSpanHits.Load(), "should be back on primary now")
}

func TestFailover_ProbeBackFails(t *testing.T) {
	primary := newConnRefusedMock()
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	// Trigger failover
	_, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Wait for a few health-check ticks
	time.Sleep(200 * time.Millisecond)

	// Active should still be on secondary since primary FetchStatus fails
	assert.Equal(t, 1, fc.registry.Active(), "should stay on secondary when primary still down")

	// Calls should still succeed via secondary
	secondaryBefore := secondary.getSpanHits.Load()
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	assert.Greater(t, secondary.getSpanHits.Load(), secondaryBefore, "should still use secondary")
}

func TestFailover_ClosesBothClients(t *testing.T) {
	var primaryClosed, secondaryClosed atomic.Bool

	primary := &mockHeimdallClient{closeFn: func() { primaryClosed.Store(true) }}
	secondary := &mockHeimdallClient{closeFn: func() { secondaryClosed.Store(true) }}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.Close()

	assert.True(t, primaryClosed.Load(), "primary should be closed")
	assert.True(t, secondaryClosed.Load(), "secondary should be closed")
}

func TestFailover_PassthroughWhenPrimaryHealthy(t *testing.T) {
	primary := &mockHeimdallClient{}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 5 * time.Second
	fc.probeTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	primaryBefore := primary.getSpanHits.Load()
	secondaryBefore := secondary.getSpanHits.Load()

	for i := 0; i < 5; i++ {
		_, err := fc.GetSpan(context.Background(), 1)
		require.NoError(t, err)
	}

	assert.Equal(t, primaryBefore+5, primary.getSpanHits.Load(), "all calls should go to primary")
	assert.Equal(t, secondaryBefore, secondary.getSpanHits.Load(), "secondary should not be contacted for API calls")
}

// Integration test using real HTTP servers to verify end-to-end behavior
func TestFailover_Integration_ServiceUnavailable(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(secondary.Close)

	primaryClient, err := NewHeimdallClient(primary.URL, 5*time.Second)
	require.NoError(t, err)
	secondaryClient, err := NewHeimdallClient(secondary.URL, 5*time.Second)
	require.NoError(t, err)

	fc, err := NewMultiHeimdallClient(primaryClient, secondaryClient)
	require.NoError(t, err)

	fc.attemptTimeout = 2 * time.Second
	defer fc.Close()

	ctx := WithRequestType(context.Background(), SpanRequest)

	// 503 should NOT trigger failover
	_, err = fc.GetSpan(ctx, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrServiceUnavailable))
}

func TestFailover_StateSyncEvents(t *testing.T) {
	primary := &mockHeimdallClient{
		stateSyncEventsFn: func(_ context.Context, _ uint64, _ int64) ([]*clerk.EventRecordWithTime, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{
		stateSyncEventsFn: func(_ context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
			return []*clerk.EventRecordWithTime{{EventRecord: clerk.EventRecord{ID: fromID}}}, nil
		},
	}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	events, err := fc.StateSyncEvents(context.Background(), 42, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, uint64(42), events[0].ID)
}

func TestFailover_GetLatestSpan(t *testing.T) {
	primary := &mockHeimdallClient{
		getLatestSpanFn: func(_ context.Context) (*types.Span, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{
		getLatestSpanFn: func(_ context.Context) (*types.Span, error) {
			return &types.Span{Id: 77}, nil
		},
	}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	span, err := fc.GetLatestSpan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(77), span.Id)
}

func TestFailover_FetchCheckpoint(t *testing.T) {
	primary := &mockHeimdallClient{
		fetchCheckpointFn: func(_ context.Context, _ int64) (*checkpoint.Checkpoint, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	cp, err := fc.FetchCheckpoint(context.Background(), 5)
	require.NoError(t, err)
	require.NotNil(t, cp)
}

func TestFailover_FetchCheckpointCount(t *testing.T) {
	primary := &mockHeimdallClient{
		fetchCheckpointCntFn: func(_ context.Context) (int64, error) {
			return 0, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	count, err := fc.FetchCheckpointCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(10), count)
}

func TestFailover_FetchMilestone(t *testing.T) {
	primary := &mockHeimdallClient{
		fetchMilestoneFn: func(_ context.Context) (*milestone.Milestone, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	ms, err := fc.FetchMilestone(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ms)
}

func TestFailover_FetchMilestoneCount(t *testing.T) {
	primary := &mockHeimdallClient{
		fetchMilestoneCntFn: func(_ context.Context) (int64, error) {
			return 0, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	count, err := fc.FetchMilestoneCount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(5), count)
}

func TestFailover_FetchStatus(t *testing.T) {
	primary := &mockHeimdallClient{
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	status, err := fc.FetchStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, status)
}

func TestFailover_SwitchOnPrimarySubContextError(t *testing.T) {
	tests := []struct {
		name      string
		primaryFn func(ctx context.Context, _ uint64) (*types.Span, error)
	}{
		{
			name: "DeadlineExceeded",
			primaryFn: func(ctx context.Context, _ uint64) (*types.Span, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		{
			name: "Canceled",
			primaryFn: func(_ context.Context, _ uint64) (*types.Span, error) {
				return nil, context.Canceled
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := &mockHeimdallClient{getSpanFn: tt.primaryFn}
			secondary := &mockHeimdallClient{}

			fc := newInstantMulti(primary, secondary)
			defer fc.Close()

			span, err := fc.GetSpan(context.Background(), 1)
			require.NoError(t, err)
			require.NotNil(t, span)
			assert.GreaterOrEqual(t, primary.getSpanHits.Load(), int32(1), "primary should have been tried")
			assert.GreaterOrEqual(t, secondary.getSpanHits.Load(), int32(1), "should failover on sub-context error")
		})
	}
}

func TestIsFailoverError(t *testing.T) {
	ctx := context.Background()

	// Transport errors should trigger failover
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	assert.True(t, isFailoverError(netErr, ctx), "net.Error should trigger failover")

	// ErrNoResponse should trigger failover
	assert.True(t, isFailoverError(ErrNoResponse, ctx), "ErrNoResponse should trigger failover")

	// 5xx HTTP errors should trigger failover; the server is unhealthy
	assert.True(t, isFailoverError(&HTTPStatusError{StatusCode: 500}, ctx), "5xx should trigger failover")
	assert.True(t, isFailoverError(fmt.Errorf("wrapped: %w", &HTTPStatusError{StatusCode: 502}), ctx), "wrapped 5xx should trigger failover")

	// 4xx HTTP errors should NOT trigger failover; a logical error will be the same on every node
	assert.False(t, isFailoverError(&HTTPStatusError{StatusCode: 400}, ctx), "4xx should not trigger failover")
	assert.False(t, isFailoverError(&HTTPStatusError{StatusCode: 404}, ctx), "4xx should not trigger failover")

	// DeadlineExceeded with live caller ctx should trigger failover
	assert.True(t, isFailoverError(context.DeadlineExceeded, ctx), "DeadlineExceeded should trigger failover when caller ctx is alive")

	// Canceled with live caller ctx should trigger failover (sub-context was canceled, not the caller)
	assert.True(t, isFailoverError(context.Canceled, ctx), "Canceled should trigger failover when caller ctx is alive")

	// ErrShutdownDetected should NOT trigger failover
	assert.False(t, isFailoverError(ErrShutdownDetected, ctx), "ErrShutdownDetected should not trigger failover")

	// ErrServiceUnavailable should NOT trigger failover
	assert.False(t, isFailoverError(ErrServiceUnavailable, ctx), "ErrServiceUnavailable should not trigger failover")

	// Caller context cancelled should NOT trigger failover
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	assert.False(t, isFailoverError(context.DeadlineExceeded, cancelledCtx), "should not failover when caller ctx is done")

	// nil error should not trigger failover
	assert.False(t, isFailoverError(nil, ctx), "nil error should not trigger failover")
}

func TestFailover_ThreeClients_CascadeToTertiary(t *testing.T) {
	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	tertiary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary, tertiary)
	defer fc.Close()

	span, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, span)

	assert.GreaterOrEqual(t, primary.getSpanHits.Load(), int32(1), "primary should have been tried")
	assert.GreaterOrEqual(t, secondary.getSpanHits.Load(), int32(1), "secondary should have been tried")
	assert.GreaterOrEqual(t, tertiary.getSpanHits.Load(), int32(1), "tertiary should have been called")
}

func TestFailover_AllClientsFail(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
	}
	secondary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
	}
	tertiary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
	}

	fc := newInstantMulti(primary, secondary, tertiary)
	defer fc.Close()

	_, err := fc.GetSpan(context.Background(), 1)
	require.Error(t, err)
}

func TestFailover_ThreeClients_ProbeBackToPrimary(t *testing.T) {
	primaryDown := atomic.Bool{}
	primaryDown.Store(true)

	primary := newToggleMock(&primaryDown)
	secondary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, testConnErr
		},
	}
	tertiary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary, tertiary)
	defer fc.Close()

	// Trigger cascade to tertiary
	_, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Bring primary back
	primaryDown.Store(false)

	// Wait for health registry to promote back to primary
	require.Eventually(t, func() bool {
		return fc.registry.Active() == 0
	}, 2*time.Second, 20*time.Millisecond, "health registry should promote back to primary")

	// Verify we're back on primary
	tertiaryBefore := tertiary.getSpanHits.Load()
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, tertiaryBefore, tertiary.getSpanHits.Load(), "should be back on primary now")
}

// Active client returns non-failover error: should return directly, no cascade.
func TestFailover_ActiveNonFailoverError(t *testing.T) {
	primary := &mockHeimdallClient{}
	secondary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, ErrShutdownDetected
		},
	}
	tertiary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary, tertiary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Force onto secondary
	fc.registry.SetActive(1)

	_, err = fc.GetSpan(context.Background(), 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrShutdownDetected))
	assert.Equal(t, int32(0), tertiary.getSpanHits.Load(), "should not cascade to tertiary on non-failover error")
}

// Active client returns failover error: cascade should try by priority.
func TestFailover_ActiveFailoverError_CascadesToNext(t *testing.T) {
	// Primary also fails so cascade doesn't land there.
	primary := newConnRefusedMock()
	secondary := newConnRefusedMock()
	tertiary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary, tertiary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.probeTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour // prevent background probes from promoting
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Force onto secondary
	fc.registry.SetActive(1)

	span, getErr := fc.GetSpan(context.Background(), 1)
	require.NoError(t, getErr)
	require.NotNil(t, span)
	assert.GreaterOrEqual(t, tertiary.getSpanHits.Load(), int32(1), "should cascade to tertiary")

	assert.Equal(t, 2, fc.registry.Active(), "active should switch to tertiary")
}

func TestFailover_ClosesAllClients(t *testing.T) {
	var closed [3]atomic.Bool

	clients := make([]Endpoint, 3)
	for i := range clients {
		idx := i
		clients[i] = &mockHeimdallClient{closeFn: func() { closed[idx].Store(true) }}
	}

	fc, err := NewMultiHeimdallClient(clients...)
	require.NoError(t, err)

	fc.Close()

	for i := range closed {
		assert.True(t, closed[i].Load(), "client %d should be closed", i)
	}
}

func TestFailover_HealthCheckPromotesHighestPriority(t *testing.T) {
	primaryDown := atomic.Bool{}
	primaryDown.Store(true)

	secondaryDown := atomic.Bool{}
	secondaryDown.Store(true)

	primary := newProbeToggleMock(&primaryDown)
	secondary := newProbeToggleMock(&secondaryDown)
	tertiary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary, tertiary)
	defer fc.Close()

	// Trigger cascade to tertiary
	_, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Bring secondary back first
	secondaryDown.Store(false)

	require.Eventually(t, func() bool {
		return fc.registry.Active() == 1
	}, 2*time.Second, 20*time.Millisecond, "should promote to secondary")

	// Now bring primary back
	primaryDown.Store(false)

	require.Eventually(t, func() bool {
		return fc.registry.Active() == 0
	}, 2*time.Second, 20*time.Millisecond, "should promote to primary")
}

func TestFailover_HealthRegistryRespectsClose(t *testing.T) {
	primary := &mockHeimdallClient{
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 50 * time.Millisecond
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0

	// Close should stop the health registry goroutine
	fc.Close()

	// No goroutine should be running after close — verify by checking
	// that probe counts don't increase after close.
	probesBefore := failoverProbeAttempts.Snapshot().Count()
	time.Sleep(200 * time.Millisecond)
	probesAfter := failoverProbeAttempts.Snapshot().Count()

	assert.Equal(t, probesBefore, probesAfter, "no probes should run after Close")
}

// --- New health registry tests ---

func TestRegistry_ConsecutiveThreshold(t *testing.T) {
	probeCount := atomic.Int32{}

	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, testConnErr
		},
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			probeCount.Add(1)
			return &ctypes.SyncInfo{}, nil
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 50 * time.Millisecond
	fc.registry.ConsecutiveThreshold = 3 // need 3 consecutive successes
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Trigger failover
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	assert.Equal(t, 1, fc.registry.Active(), "should be on secondary")

	// Wait for enough probes to pass the threshold
	require.Eventually(t, func() bool {
		return probeCount.Load() >= 3
	}, 2*time.Second, 20*time.Millisecond, "should probe primary at least 3 times")

	// Should eventually promote after threshold met
	require.Eventually(t, func() bool {
		return fc.registry.Active() == 0
	}, 2*time.Second, 20*time.Millisecond, "should promote after consecutive threshold met")
}

func TestRegistry_PromotionCooldown(t *testing.T) {
	primaryDown := atomic.Bool{}
	primaryDown.Store(true)

	primary := newProbeToggleMock(&primaryDown)
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 50 * time.Millisecond
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 500 * time.Millisecond // 500ms cooldown
	defer fc.Close()

	// Trigger failover
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Bring primary back
	primaryDown.Store(false)

	// Wait for at least one probe to succeed — primary should be healthy but not promoted yet
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 1, fc.registry.Active(), "should not promote before cooldown")

	// Wait for cooldown to pass and promotion to happen
	require.Eventually(t, func() bool {
		return fc.registry.Active() == 0
	}, 3*time.Second, 20*time.Millisecond, "should promote after cooldown passes")
}

func TestRegistry_FlappingPrevention(t *testing.T) {
	callCount := atomic.Int32{}

	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return nil, testConnErr
		},
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			n := callCount.Add(1)
			// Alternate: success, fail, success, fail...
			if n%2 == 0 {
				return nil, testConnErr
			}
			return &ctypes.SyncInfo{}, nil
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 50 * time.Millisecond
	fc.registry.ConsecutiveThreshold = 3
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Trigger failover
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Wait for several probe cycles
	time.Sleep(500 * time.Millisecond)

	// Primary should never reach healthy because alternating success/fail
	// never reaches 3 consecutive successes.
	assert.Equal(t, 1, fc.registry.Active(), "should stay on secondary — flapping primary never reaches threshold")
}

func TestRegistry_InformedCascade_SkipsUnhealthy(t *testing.T) {
	primary := newConnRefusedMock()
	secondary := newConnRefusedMock()
	tertiary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary, tertiary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Mark secondary as unhealthy in the registry
	fc.registry.SetHealth(1, EndpointHealth{Healthy: false})

	// Trigger failover from primary
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)

	// Secondary should not have been tried for the GetSpan call since it's unhealthy,
	// but it may be tried in the last-resort pass. The key thing is that tertiary succeeds.
	assert.Equal(t, 2, fc.registry.Active(), "should end up on tertiary")
}

func TestRegistry_InformedCascade_TriesByPriority(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	// Track call order
	var callOrder []int
	var orderMu sync.Mutex

	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			orderMu.Lock()
			callOrder = append(callOrder, 0)
			orderMu.Unlock()
			return &types.Span{Id: 1}, nil
		},
	}
	secondary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			orderMu.Lock()
			callOrder = append(callOrder, 1)
			orderMu.Unlock()
			return nil, connErr
		},
	}
	tertiary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			orderMu.Lock()
			callOrder = append(callOrder, 2)
			orderMu.Unlock()
			return nil, connErr
		},
	}

	fc, err := NewMultiHeimdallClient(primary, secondary, tertiary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Force active to index 1 (secondary); primary (index 0) is healthy
	fc.registry.SetActive(1)
	fc.registry.SetHealth(0, EndpointHealth{Healthy: true, HealthySince: time.Now().Add(-1 * time.Hour)})
	fc.registry.SetHealth(1, EndpointHealth{Healthy: true})
	fc.registry.SetHealth(2, EndpointHealth{Healthy: true})

	span, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, span)

	// Cascade should try primary (index 0) before tertiary (index 2)
	assert.Equal(t, 0, fc.registry.Active(), "should cascade to primary (highest priority)")
}

func TestRegistry_ProactiveSwitchOnActiveUnhealthy(t *testing.T) {
	primaryDown := atomic.Bool{}
	primaryDown.Store(false)

	primary := &mockHeimdallClient{
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) {
			if primaryDown.Load() {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return &ctypes.SyncInfo{}, nil
		},
	}
	secondary := &mockHeimdallClient{}

	fc := newInstantMulti(primary, secondary)
	defer fc.Close()

	// Start the health registry (normally started on first API call).
	fc.ensureHealthRegistry()

	// Verify we start on primary
	assert.Equal(t, 0, fc.registry.Active(), "should start on primary")

	// Now make primary go down — the health registry should detect and switch
	primaryDown.Store(true)

	require.Eventually(t, func() bool {
		return fc.registry.Active() == 1
	}, 2*time.Second, 20*time.Millisecond, "health registry should proactively switch to secondary")
}

func TestRegistry_CascadeFallsBackToUnhealthy(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
	}
	// Secondary is marked unhealthy but actually works
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.probeTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Start registry and let the immediate probe complete before setting up
	// the test state, otherwise the probe can mark secondary healthy.
	fc.ensureHealthRegistry()
	time.Sleep(50 * time.Millisecond)

	// Mark secondary as unhealthy
	fc.registry.SetHealth(1, EndpointHealth{Healthy: false})

	// Primary fails, cascade should fall back to unhealthy secondary as last resort
	span, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, span)

	assert.Equal(t, 1, fc.registry.Active(), "should fall back to unhealthy secondary as last resort")
}

func TestRegistry_MarkUnhealthyOnRealFailure(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	// Primary must fail BOTH GetSpan and the FetchStatus health probe — otherwise
	// the registry's immediate probe cycle (launched by ensureHealthRegistry) can
	// race with this test's GetSpan and mark primary healthy after MarkUnhealthy.
	primary := &mockHeimdallClient{
		getSpanFn:     func(_ context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
		fetchStatusFn: func(_ context.Context) (*ctypes.SyncInfo, error) { return nil, connErr },
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	// Primary starts as healthy
	snap := fc.registry.HealthSnapshot()
	assert.True(t, snap[0].Healthy, "primary should start healthy")

	// Trigger a real request that fails on primary
	_, err = fc.GetSpan(context.Background(), 1)
	require.NoError(t, err) // succeeds via secondary

	// Primary should now be marked unhealthy
	snap = fc.registry.HealthSnapshot()
	assert.False(t, snap[0].Healthy, "primary should be marked unhealthy after real failure")
	assert.Equal(t, 0, snap[0].ConsecutiveSuccess, "consecutive success should be reset")
}

func TestFailover_ProbeUsesProbeTimeout(t *testing.T) {
	// Verify that probes use the short probeTimeout, not the long attemptTimeout.
	// A probe against a hanging endpoint should fail within probeTimeout, not
	// wait for attemptTimeout.
	primary := &mockHeimdallClient{
		fetchStatusFn: func(ctx context.Context) (*ctypes.SyncInfo, error) {
			// Hang until context expires.
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	secondary := &mockHeimdallClient{}

	fc, err := NewMultiHeimdallClient(primary, secondary)
	require.NoError(t, err)

	fc.attemptTimeout = 10 * time.Second // long — should NOT be used for probes
	fc.probeTimeout = 200 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 0
	defer fc.Close()

	start := time.Now()
	fc.registry.Start()

	// Wait for the immediate probe cycle to complete.
	require.Eventually(t, func() bool {
		snap := fc.registry.HealthSnapshot()
		return !snap[0].Healthy || snap[0].LastErr != nil
	}, 2*time.Second, 20*time.Millisecond, "probe should complete")

	elapsed := time.Since(start)
	assert.Less(t, elapsed, 2*time.Second, "probe should complete within probeTimeout, not attemptTimeout")
}

func TestRegistry_InformedCascade_RespectsCooldown(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	// Primary (index 0): healthy but NOT cooled (recently became healthy)
	// Secondary (index 1): fails (active)
	// Tertiary (index 2): healthy AND cooled

	primary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return &types.Span{Id: 1}, nil
		},
	}
	secondary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) { return nil, connErr },
	}
	tertiary := &mockHeimdallClient{
		getSpanFn: func(_ context.Context, _ uint64) (*types.Span, error) {
			return &types.Span{Id: 3}, nil
		},
	}

	fc, err := NewMultiHeimdallClient(primary, secondary, tertiary)
	require.NoError(t, err)

	fc.attemptTimeout = 100 * time.Millisecond
	fc.registry.HealthCheckInterval = 1 * time.Hour
	fc.registry.ConsecutiveThreshold = 1
	fc.registry.PromotionCooldown = 1 * time.Hour // long cooldown
	defer fc.Close()

	// Set up health states
	fc.registry.SetActive(1)
	fc.registry.SetHealth(0, EndpointHealth{Healthy: true, HealthySince: time.Now()})                     // NOT cooled
	fc.registry.SetHealth(1, EndpointHealth{Healthy: true})                                               // active, will fail
	fc.registry.SetHealth(2, EndpointHealth{Healthy: true, HealthySince: time.Now().Add(-2 * time.Hour)}) // cooled

	span, err := fc.GetSpan(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, span)

	// Should prefer tertiary (cooled) over primary (uncooled)
	assert.Equal(t, 2, fc.registry.Active(), "should prefer cooled tertiary over uncooled primary")
}
