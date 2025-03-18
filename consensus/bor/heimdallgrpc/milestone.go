package heimdallgrpc

import (
	"context"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/log"

	protoutils "github.com/maticnetwork/polyproto/utils"
)

func (h *HeimdallGRPCClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	log.Info("Fetching milestone count")

	res, err := h.client.FetchMilestoneCount(ctx, nil)
	if err != nil {
		return 0, err
	}

	log.Info("Fetched milestone count")

	return res.Result.Count, nil
}

func (h *HeimdallGRPCClient) FetchMilestone(ctx context.Context) (*milestone.MilestoneV2, error) {
	log.Info("Fetching milestone")

	res, err := h.client.FetchMilestone(ctx, nil)
	if err != nil {
		return nil, err
	}

	log.Info("Fetched milestone")

	milestone := &milestone.MilestoneV2{
		StartBlock: res.Result.StartBlock,
		EndBlock:   res.Result.EndBlock,
		Hash:       protoutils.ConvertH256ToHash(res.Result.RootHash),
		Proposer:   protoutils.ConvertH160toAddress(res.Result.Proposer),
		BorChainID: res.Result.BorChainID,
		Timestamp:  uint64(res.Result.Timestamp.GetSeconds()),
	}

	return milestone, nil
}
