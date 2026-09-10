package rpc

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/metrics"
)

// SafePool caps concurrent execution of submitted functions: a non-zero size uses
// a counting semaphore, size 0 is an unbounded fast path. It's a semaphore rather
// than a worker pool on purpose — a dispatcher that exits (e.g. on Stop) would
// wedge every later Submit; a semaphore can't.
type SafePool struct {
	// concurrency limiter; cap == size, nil on the fast path. Swapped by ChangeSize.
	sem atomic.Pointer[chan struct{}]

	inFlight  atomic.Int64 // running tasks, for metrics
	processed atomic.Int64 // tasks completed since the last metrics report

	sync.RWMutex
	size    int
	timeout time.Duration
	service string

	close     chan struct{}
	closeOnce sync.Once

	// per-task goroutine instead of a slot; atomic because Submit reads it lock-free.
	fastPath atomic.Bool
}

func NewExecutionPool(initialSize int, timeout time.Duration, service string, report bool) *SafePool {
	sp := &SafePool{
		size:    initialSize,
		timeout: timeout,
		service: service,
		close:   make(chan struct{}),
	}

	if initialSize <= 0 {
		sp.fastPath.Store(true)

		return sp
	}

	sem := make(chan struct{}, initialSize)
	sp.sem.Store(&sem)

	if metrics.Enabled() && report {
		go sp.reportMetrics(3 * time.Second)
	}

	return sp
}

// Submit runs fn, bounded to the pool size (fast path: unbounded). fn always
// runs — the caller's WaitGroup counts on it — so at capacity it waits for a
// slot, falling back to an unbounded run if ctx is cancelled or the configured
// timeout elapses, so a saturated pool can't block the caller indefinitely.
func (s *SafePool) Submit(ctx context.Context, fn func() error) (<-chan error, bool) {
	s.SubmitWithSlot(ctx, func(*Slot) error {
		return fn()
	})

	return nil, true
}

// SubmitWithSlot is Submit for tasks that manage their own hold on the pool: fn
// receives the slot acquired for it and may hand it back for the duration of a
// wait that isn't work of its own. The slot is nil when the task runs unbounded
// (fast path, or the saturation fallback), and a nil slot's methods are no-ops.
func (s *SafePool) SubmitWithSlot(ctx context.Context, fn func(*Slot) error) {
	unbounded := func() {
		go func() {
			_ = fn(nil)
		}()
	}

	semPtr := s.sem.Load()
	if s.fastPath.Load() || semPtr == nil {
		unbounded()

		return
	}

	sem := *semPtr

	var timeout <-chan time.Time
	if d := s.Timeout(); d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case sem <- struct{}{}: // acquired a slot
		s.inFlight.Add(1)

		slot := &Slot{pool: s, sem: sem, held: true}

		go func() {
			defer slot.Release()

			_ = fn(slot)
		}()
	case <-ctx.Done():
		unbounded()
	case <-timeout:
		unbounded()
	}
}

func (s *SafePool) ChangeSize(n int) {
	if n <= 0 {
		// no semaphore at size 0 — use the fast path
		s.fastPath.Store(true)

		s.Lock()
		s.size = 0
		s.Unlock()

		return
	}

	sem := make(chan struct{}, n)
	// store the semaphore before clearing fastPath, so Submit never sees
	// fastPath=false with no semaphore; in-flight tasks release into the old one
	s.sem.Store(&sem)
	s.fastPath.Store(false)

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
	s.closeOnce.Do(func() {
		close(s.close)
	})
}

// reportMetrics reports execution-pool metrics every refresh interval.
func (s *SafePool) reportMetrics(refresh time.Duration) {
	ticker := time.NewTicker(refresh)

	for {
		select {
		case <-ticker.C:
			workerCount, waitingQueue, processed := newEpMetrics(s.service)

			workerCount.Update(s.inFlight.Load())
			waitingQueue.Update(0)
			processed.Update(s.processed.Load())

			s.processed.Store(0)
		case <-s.close:
			ticker.Stop()

			return
		}
	}
}
