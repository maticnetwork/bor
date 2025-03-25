package heimdallgrpc

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/log"

	checkpointTypes "github.com/0xPolygon/heimdall-v2/x/checkpoint/types"
)

func (h *HeimdallGRPCClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	panic("Not implemented!")
}

func (h *HeimdallGRPCClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	req := &checkpointTypes.QueryCheckpointRequest{
		Number: uint64(number),
	}

	log.Info("Fetching checkpoint", "number", number)

	res, err := h.checkpointQueryClient.GetCheckpoint(ctx, req)
	if err != nil {
		return nil, err
	}

	resCheckpoint := res.GetCheckpoint()

	log.Info("Fetched checkpoint", "number", number)

	checkpoint := &checkpoint.Checkpoint{
		Proposer:   common.HexToAddress(resCheckpoint.Proposer),
		StartBlock: resCheckpoint.StartBlock,
		EndBlock:   resCheckpoint.EndBlock,
		RootHash:   common.BytesToHash(resCheckpoint.RootHash),
		BorChainID: resCheckpoint.BorChainId,
		Timestamp:  resCheckpoint.Timestamp,
	}

	return checkpoint, nil
}
