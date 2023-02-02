package rpc

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/JekaMas/workerpool"
)

type SafePool struct {
	executionPool *atomic.Pointer[workerpool.WorkerPool]
	fastPath      bool
	timeout       *atomic.Pointer[time.Duration]
}

func NewExecutionPool(initialSize int, timeout time.Duration) *SafePool {
	if initialSize == 0 {
		return &SafePool{fastPath: true}
	}

	var ptr atomic.Pointer[workerpool.WorkerPool]
	var ptrTimeout atomic.Pointer[time.Duration]

	p := workerpool.New(initialSize)
	ptr.Store(p)

	ptrTimeout.Store(&timeout)

	return &SafePool{executionPool: &ptr, timeout: &ptrTimeout}

}

func (s *SafePool) Submit(ctx context.Context, fn func() error, timeout time.Duration) (<-chan error, bool) {
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

	return pool.Submit(ctx, fn, timeout), true
}

func (s *SafePool) ChangeSize(n int) {
	oldPool := s.executionPool.Swap(workerpool.New(n))

	if oldPool != nil {
		go func() {
			oldPool.StopWait()
		}()
	}
}

func (s *SafePool) ChangeTimeout(n time.Duration) {
	s.timeout.Swap(&n)
}
