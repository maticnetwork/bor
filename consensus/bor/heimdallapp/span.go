package heimdallapp

import (
	"context"
	"encoding/json"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"

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

	resBytes, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}

	var span span.HeimdallSpan

	err = json.Unmarshal(resBytes, &span)
	if err != nil {
		return nil, err
	}

	return &span, nil
}
