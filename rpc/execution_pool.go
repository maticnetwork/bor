package rpc

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/JekaMas/workerpool"
)

const (
	threads        = 100
	requestTimeout = 0 //10 * time.Second
)

type SafePool struct {
	executionPool *atomic.Pointer[workerpool.WorkerPool]
}

func NewExecutionPool(initialSize int) *SafePool {
	if initialSize == 0 {
		return &SafePool{}
	}

	var ptr atomic.Pointer[workerpool.WorkerPool]

	p := workerpool.New(initialSize)
	ptr.Store(p)

	return &SafePool{&ptr}
}

func (s *SafePool) Submit(ctx context.Context, fn func() error, timeout ...time.Duration) (<-chan error, bool) {
	pool := s.executionPool.Load()
	if pool != nil {
		return pool.Submit(ctx, fn, timeout...), true
	}

	go func() {
		_ = fn()
	}()

	return nil, false
}

func (s *SafePool) ChangeSize(n int) {
	oldPool := s.executionPool.Swap(workerpool.New(n))

	if oldPool != nil {
		go func() {
			oldPool.StopWait()
		}()
	}
}
