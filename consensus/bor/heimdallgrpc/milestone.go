package heimdallgrpc

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/log"
)

func (h *HeimdallGRPCClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	log.Info("Fetching milestone count")

	res, err := h.milestoneQueryClient.GetMilestoneCount(ctx, nil)
	if err != nil {
		return 0, err
	}

	// Get the count from the response
	count := res.GetCount()

	log.Info("Fetched milestone count", "count", count)

	return int64(count), nil
}

func (h *HeimdallGRPCClient) FetchMilestone(ctx context.Context) (*milestone.Milestone, error) {
	log.Info("Fetching milestone")

	res, err := h.milestoneQueryClient.GetLatestMilestone(ctx, nil)
	if err != nil {
		return nil, err
	}

	resMilestone := res.GetMilestone()

	log.Info("Fetched milestone")

	milestone := &milestone.Milestone{
		Proposer:   common.HexToAddress(resMilestone.Proposer),
		StartBlock: resMilestone.StartBlock,
		EndBlock:   resMilestone.EndBlock,
		Hash:       common.BytesToHash(resMilestone.Hash),
		BorChainID: resMilestone.BorChainId,
		Timestamp:  resMilestone.Timestamp,
	}

	return milestone, nil
}
