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

	// Start the timer and set the request type on the context.
	start := time.Now()
	ctx = heimdall.WithRequestType(ctx, heimdall.SpanRequest)

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
