package heimdallapp

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"

	"github.com/ethereum/go-ethereum/log"

	milestoneTypes "github.com/0xPolygon/heimdall-v2/x/milestone/types"
)

func (h *HeimdallAppClient) FetchMilestoneCount(_ context.Context) (int64, error) {
	log.Debug("Fetching milestone count")

	res, err := h.hApp.MilestoneKeeper.GetMilestoneCount(h.NewContext())
	if err != nil {
		return 0, err
	}

	log.Debug("Fetched Milestone Count", "res", int64(res))

	return int64(res), nil
}

func (h *HeimdallAppClient) FetchMilestone(_ context.Context) (*milestone.MilestoneV2, error) {
	log.Debug("Fetching Latest Milestone")

	res, err := h.hApp.MilestoneKeeper.GetLastMilestone(h.NewContext())
	if err != nil {
		return nil, err
	}

	milestone := toBorMilestone(res)
	log.Debug("Fetched Latest Milestone", "milestone", milestone)

	return milestone, nil
}

func toBorMilestone(hdMilestone *milestoneTypes.Milestone) *milestone.MilestoneV2 {
	return &milestone.MilestoneV2{
		Proposer:   common.HexToAddress(hdMilestone.Proposer),
		StartBlock: hdMilestone.StartBlock,
		EndBlock:   hdMilestone.EndBlock,
		Hash:       common.BytesToHash(hdMilestone.Hash),
		BorChainID: hdMilestone.BorChainId,
		Timestamp:  hdMilestone.Timestamp,
	}
}
