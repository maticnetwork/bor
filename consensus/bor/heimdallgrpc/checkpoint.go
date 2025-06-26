package heimdallgrpc

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/log"

	checkpointTypes "github.com/0xPolygon/heimdall-v2/x/checkpoint/types"
)

func (h *HeimdallGRPCClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	log.Info("Fetching checkpoint count")

	var err error

	// Start the timer and set the request type on the context.
	start := time.Now()
	ctx = heimdall.WithRequestType(ctx, heimdall.CheckpointCountRequest)

	// Defer the metrics call.
	defer func() {
		heimdall.SendMetrics(ctx, start, err == nil)
	}()

	res, err := h.checkpointQueryClient.GetAckCount(ctx, nil)
	if err != nil {
		return 0, err
	}

	count := int64(res.GetAckCount())

	log.Info("Fetched checkpoint count", "count", count)

	return count, nil
}

func (h *HeimdallGRPCClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	var fetchedCheckpoint checkpointTypes.Checkpoint
	var err error

	// Start the timer and set the request type on the context.
	start := time.Now()
	ctx = heimdall.WithRequestType(ctx, heimdall.CheckpointRequest)

	// Defer the metrics call.
	defer func() {
		heimdall.SendMetrics(ctx, start, err == nil)
	}()

	if number == -1 {
		// If -1 is passed, we will fetch the latest checkpoint.
		log.Info("Fetching latest checkpoint")

		res, err := h.checkpointQueryClient.GetCheckpointLatest(ctx, nil)
		if err != nil {
			return nil, err
		}
		fetchedCheckpoint = res.GetCheckpoint()

		log.Info("Fetched latest checkpoint")
	} else {
		// Else, we will fetch the checkpoint by number.
		log.Info("Fetching checkpoint", "number", number)

		req := &checkpointTypes.QueryCheckpointRequest{
			Number: uint64(number),
		}
		res, err := h.checkpointQueryClient.GetCheckpoint(ctx, req)
		if err != nil {
			return nil, err
		}
		fetchedCheckpoint = res.GetCheckpoint()

		log.Info("Fetched checkpoint", "number", number)
	}

	checkpoint := &checkpoint.Checkpoint{
		Proposer:   common.HexToAddress(fetchedCheckpoint.Proposer),
		StartBlock: fetchedCheckpoint.StartBlock,
		EndBlock:   fetchedCheckpoint.EndBlock,
		RootHash:   common.BytesToHash(fetchedCheckpoint.RootHash),
		BorChainID: fetchedCheckpoint.BorChainId,
		Timestamp:  fetchedCheckpoint.Timestamp,
	}

	return checkpoint, nil
}
