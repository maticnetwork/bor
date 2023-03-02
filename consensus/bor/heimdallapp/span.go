package heimdallapp

import (
	"context"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"

	jsoniter "github.com/json-iterator/go"
	abci "github.com/tendermint/tendermint/abci/types"
)

func (h *HeimdallAppClient) Span(ctx context.Context, spanID uint64) (*span.HeimdallSpan, error) {
	log.Info("Fetching span", "spanID", spanID)

	hCtx := h.hApp.NewContext(true, abci.Header{Height: h.hApp.LastBlockHeight()})

	res, err := h.hApp.BorKeeper.GetSpan(hCtx, spanID)
	if err != nil {
		return nil, err
	}

	log.Info("Fetched span", "spanID", spanID)

	resBytes, err := jsoniter.ConfigFastest.Marshal(res)
	if err != nil {
		return nil, err
	}

	var span span.HeimdallSpan

	err = jsoniter.ConfigFastest.Unmarshal(resBytes, &span)
	if err != nil {
		return nil, err
	}

	return &span, nil
}
