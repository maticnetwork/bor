//+go:build go1.19

package log

import "sync/atomic"

// swapHandler wraps another handler that may be swapped out
// dynamically at runtime in a thread-safe fashion.
type swapHandler[T Handler] struct {
	handler atomic.Pointer[T]
}

func (h *swapHandler[T]) Log(r *Record) error {
	return (*h.handler.Load()).Log(r)
}

func (h *swapHandler[T]) Swap(newHandler T) {
	h.handler.Store(&newHandler)
}

func (h *swapHandler[T]) Get() T {
	return *h.handler.Load()
}
