package heimdallapp

import (
	"context"
	"math/big"

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

func (h *HeimdallAppClient) FetchMilestone(_ context.Context) (*milestone.Milestone, error) {
	log.Debug("Fetching Latest Milestone")

	res, err := h.hApp.MilestoneKeeper.GetLastMilestone(h.NewContext())
	if err != nil {
		return nil, err
	}

	milestone := toBorMilestone(res)
	log.Debug("Fetched Latest Milestone", "milestone", milestone)

	return milestone, nil
}

// func (h *HeimdallAppClient) FetchNoAckMilestone(_ context.Context, milestoneID string) error {
// 	log.Debug("Fetching No Ack Milestone By MilestoneID", "MilestoneID", milestoneID)

// 	res := h.hApp.CheckpointKeeper.GetNoAckMilestone(h.NewContext(), milestoneID)
// 	if res {
// 		log.Info("Fetched No Ack By MilestoneID", "MilestoneID", milestoneID)
// 		return nil
// 	}

// 	return fmt.Errorf("still no-ack milestone exist corresponding to milestoneID: %v", milestoneID)
// }

func (h *HeimdallAppClient) FetchLastNoAckMilestone(_ context.Context) (string, error) {
	log.Debug("Fetching Latest No Ack Milestone ID")

	res, err := h.hApp.MilestoneKeeper.GetLastNoAckMilestone(h.NewContext())
	if err != nil {
		return "", err
	}

	log.Debug("Fetched Latest No Ack Milestone ID", "res", res)

	return res, nil
}

// func (h *HeimdallAppClient) FetchMilestoneID(_ context.Context, milestoneID string) error {
// 	log.Debug("Fetching Milestone ID ", "MilestoneID", milestoneID)

// 	res := milestoneTypes.GetMilestoneID()

// 	if res == milestoneID {
// 		return nil
// 	}

// 	return fmt.Errorf("milestone corresponding to milestoneID: %v doesn't exist in heimdall", milestoneID)
// }

func toBorMilestone(hdMilestone *milestoneTypes.Milestone) *milestone.Milestone {
	return &milestone.Milestone{
		Proposer:   common.HexToAddress(hdMilestone.Proposer),
		StartBlock: big.NewInt(int64(hdMilestone.StartBlock)),
		EndBlock:   big.NewInt(int64(hdMilestone.EndBlock)),
		Hash:       common.BytesToHash(hdMilestone.Hash),
		BorChainID: hdMilestone.BorChainId,
		Timestamp:  hdMilestone.Timestamp,
	}
}
