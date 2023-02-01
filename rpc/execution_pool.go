package rpc

import (
	"context"
	"sync/atomic"

	"github.com/JekaMas/workerpool"
)

const (
	threads        = 100
	requestTimeout = 0 //10 * time.Second
)

var execPool atomic.Pointer[workerpool.WorkerPool]

func init() {
	changePoolSize(threads)
}

func changePoolSize(n int) {
	oldPool := execPool.Load()

	if oldPool != nil {
		go func() {
			oldPool.StopWait()
		}()
	}

	execPool.Store(workerpool.New(n))
}

func Run(runFn func()) {
	fn := func() error {
		runFn()

		return nil
	}

	ctx := context.Background()

	if requestTimeout > 0 {
		execPool.Load().Submit(ctx, fn, requestTimeout)
	} else {
		execPool.Load().Submit(ctx, fn)
	}
}
