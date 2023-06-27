package rpc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JekaMas/workerpool"
	"github.com/ethereum/go-ethereum/metrics"
)

type SafePool struct {
	executionPool *atomic.Pointer[workerpool.WorkerPool]

	sync.RWMutex

	timeout   time.Duration
	size      int
	close     chan struct{}
	processed atomic.Int64

	// Skip sending task to execution pool
	fastPath bool
}

func NewExecutionPool(initialSize int, timeout time.Duration) *SafePool {
	sp := &SafePool{
		size:    initialSize,
		timeout: timeout,
		close:   make(chan struct{}),
	}

	if initialSize == 0 {
		sp.fastPath = true

		return sp
	}

	var ptr atomic.Pointer[workerpool.WorkerPool]

	p := workerpool.New(initialSize)
	ptr.Store(p)
	sp.executionPool = &ptr

	return sp
}

func (s *SafePool) Submit(ctx context.Context, fn func() error) (<-chan error, bool) {
	if s.fastPath {
		go func() {
			_ = fn()
		}()

		return nil, true
	}

	if s.executionPool == nil {
		return nil, false
	}

	pool := s.executionPool.Load()
	if pool == nil {
		return nil, false
	}

	return pool.Submit(ctx, fn, s.Timeout()), true
}

func (s *SafePool) ChangeSize(n int) {
	oldPool := s.executionPool.Swap(workerpool.New(n))

	if oldPool != nil {
		go func() {
			oldPool.StopWait()
		}()
	}

	s.Lock()
	s.size = n
	s.Unlock()
}

func (s *SafePool) ChangeTimeout(n time.Duration) {
	s.Lock()
	defer s.Unlock()

	s.timeout = n
}

func (s *SafePool) Timeout() time.Duration {
	s.RLock()
	defer s.RUnlock()

	return s.timeout
}

func (s *SafePool) Size() int {
	s.RLock()
	defer s.RUnlock()

	return s.size
}

func (s *SafePool) Stop() {
	close(s.close)
	s.executionPool.Load().Stop()
}

// reportMetrics reports the metrics after every `refresh` time interval
// regarding the execution pool.
func (s *SafePool) reportMetrics(refresh time.Duration) {
	if !metrics.Enabled {
		return
	}

	ticker := time.NewTicker(refresh)
	for {
		select {
		case <-ticker.C:
			epWorkerCountGuage.Update(s.executionPool.Load().GetWorkerCount())
			epWaitingQueueGuage.Update(int64(s.executionPool.Load().WaitingQueueSize()))
			epProcessedRequestsMeter.Mark(s.processed.Load())

			s.processed.Store(0)
		case <-s.close:
			ticker.Stop()

			return
		}
	}
}
