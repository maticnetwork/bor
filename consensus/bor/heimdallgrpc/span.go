package heimdallgrpc

import (
	"context"
	"strconv"

	"github.com/ethereum/go-ethereum/log"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
)

func (h *HeimdallGRPCClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	log.Info("Fetching span", "spanID", spanID)

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
