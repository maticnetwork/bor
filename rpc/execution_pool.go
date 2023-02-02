package rpc

import (
	"sync/atomic"
	"time"

	"github.com/JekaMas/workerpool"
)

const (
	threads        = 100
	requestTimeout = 10 * time.Second
)

//nolint:unused
func changePoolSize(execPool *atomic.Pointer[workerpool.WorkerPool], n int) {
	oldPool := execPool.Load()

	if oldPool != nil {
		go func() {
			oldPool.StopWait()
		}()
	}

	execPool.Store(workerpool.New(n))
}
