package heimdallapp

import (
	"context"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"

	"github.com/ethereum/go-ethereum/log"
)

func (h *HeimdallAppClient) GetSpan(_ context.Context, _ uint64) (*borTypes.Span, error) {
	log.Warn("GetSpan not implemented!")
	return nil, nil
}
