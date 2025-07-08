package heimdallgrpc

import (
	"context"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall"
	"github.com/ethereum/go-ethereum/log"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
)

func (h *HeimdallGRPCClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	log.Info("Fetching span", "spanID", spanID)

	var err error

	ctxWithTimeout, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	// Start the timer and set the request type on the context.
	start := time.Now()
	ctx = heimdall.WithRequestType(ctxWithTimeout, heimdall.SpanRequest)

	// Defer the metrics call.
	defer func() {
		heimdall.SendMetrics(ctx, start, err == nil)
	}()

	req := &types.QuerySpanByIdRequest{
		Id: strconv.FormatUint(spanID, 10),
	}

	res, err := h.borQueryClient.GetSpanById(ctx, req)
	if err != nil {
		return nil, err
	}

	resSpan := res.GetSpan()

	log.Info("Fetched span", "spanID", spanID)

	return resSpan, nil
}

func (h *HeimdallGRPCClient) GetLatestSpan(ctx context.Context) (*types.Span, error) {
	log.Info("Fetching latest span")

	var err error

	ctxWithTimeout, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	// Start the timer and set the request type on the context.
	start := time.Now()
	ctx = heimdall.WithRequestType(ctxWithTimeout, heimdall.LatestSpanRequest)

	// Defer the metrics call.
	defer func() {
		heimdall.SendMetrics(ctx, start, err == nil)
	}()

	req := &types.QueryLatestSpanRequest{}

	res, err := h.borQueryClient.GetLatestSpan(ctx, req)
	if err != nil {
		return nil, err
	}

	resSpan := res.GetSpan()

	log.Info("Fetched latest span")

	return &resSpan, nil
}
