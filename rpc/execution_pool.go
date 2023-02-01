package rpc

import (
	"context"
	"time"

	"github.com/JekaMas/workerpool"
)

const (
	threads        = 100
	requestTimeout = 10 * time.Second
)

var execPool = workerpool.New(threads)

func Run(runFn func()) {
	fn := func() error {
		runFn()

		return nil
	}

	ctx := context.Background()

	if requestTimeout > 0 {
		execPool.Submit(ctx, fn, requestTimeout)
	} else {
		execPool.Submit(ctx, fn)
	}
}
