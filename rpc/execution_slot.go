package rpc

import "context"

// Slot is a task's hold on an execution pool. A handler that blocks on an
// external event rather than on work of its own can hand the slot back for the
// duration of the wait: a parked goroutine costs a few kilobytes, while a slot
// is a fraction of the node's entire RPC concurrency budget.
//
// A slot belongs to the single goroutine SubmitWithSlot handed it to. Release
// and Acquire are idempotent but not safe to call concurrently with each other.
type Slot struct {
	pool *SafePool
	sem  chan struct{} // the semaphore this hold came from; ChangeSize may since have swapped it
	held bool
}

// Release hands the slot back to the pool.
func (s *Slot) Release() {
	if s == nil || !s.held {
		return
	}

	s.held = false
	<-s.sem
	s.pool.inFlight.Add(-1)
}

// Acquire takes a slot back, from the pool's current semaphore so a task that
// waited through a ChangeSize rejoins under the new bound. It is best effort: a
// saturated pool lets the caller proceed rather than parking it, because
// callers that release a slot are bounded by their own admission limit, and
// blocking here would stop them draining the events they were waiting on.
func (s *Slot) Acquire() {
	if s == nil || s.held {
		return
	}

	semPtr := s.pool.sem.Load()
	if s.pool.fastPath.Load() || semPtr == nil {
		return // the pool went unbounded while we were waiting
	}

	sem := *semPtr

	select {
	case sem <- struct{}{}:
		s.sem = sem
		s.held = true
		s.pool.inFlight.Add(1)
	default:
	}
}

type slotKey struct{}

// withExecutionSlot publishes the task's hold for its handler. A nil slot is
// stored as-is: it reads back as a nil *Slot, whose methods are no-ops.
func withExecutionSlot(ctx context.Context, slot *Slot) context.Context {
	return context.WithValue(ctx, slotKey{}, slot)
}

// ExecutionSlotFromContext returns the calling handler's hold on the execution
// pool, or nil when the call isn't running under a bounded one. Callers don't
// need the nil check — a nil slot's methods are no-ops.
func ExecutionSlotFromContext(ctx context.Context) *Slot {
	slot, _ := ctx.Value(slotKey{}).(*Slot)

	return slot
}
