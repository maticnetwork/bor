package heimdallapp

import (
	"context"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/log"

	jsoniter "github.com/json-iterator/go"
	abci "github.com/tendermint/tendermint/abci/types"
)

func (h *HeimdallAppClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	log.Info("Fetching checkpoint count")

	hCtx := h.hApp.NewContext(true, abci.Header{Height: h.hApp.LastBlockHeight()})

	res := h.hApp.CheckpointKeeper.GetACKCount(hCtx)

	log.Info("Fetched checkpoint count")

	return int64(res), nil
}

func (h *HeimdallAppClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	log.Info("Fetching checkpoint", "number", number)

	hCtx := h.hApp.NewContext(true, abci.Header{Height: h.hApp.LastBlockHeight()})

	res, err := h.hApp.CheckpointKeeper.GetCheckpointByNumber(hCtx, uint64(number))
	if err != nil {
		return nil, err
	}

	log.Info("Fetched checkpoint", "number", number)

	resBytes, err := jsoniter.ConfigFastest.Marshal(res)
	if err != nil {
		return nil, err
	}

	var checkpoint checkpoint.Checkpoint

	err = jsoniter.ConfigFastest.Unmarshal(resBytes, &checkpoint)
	if err != nil {
		return nil, err
	}

	return &checkpoint, nil
}
