package heimdallgrpc

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/log"

	proto "github.com/maticnetwork/polyproto/heimdall"
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

func (h *HeimdallGRPCClient) FetchMilestone(ctx context.Context, number int64) (*milestone.Milestone, error) {
	log.Info("Fetching milestone")

	res, err := h.client.FetchMilestone(ctx, nil)
	if err != nil {
		return nil, err
	}

	log.Info("Fetched milestone")

	milestone := &milestone.Milestone{
		StartBlock: new(big.Int).SetUint64(res.Result.StartBlock),
		EndBlock:   new(big.Int).SetUint64(res.Result.EndBlock),
		RootHash:   protoutils.ConvertH256ToHash(res.Result.RootHash),
		Proposer:   protoutils.ConvertH160toAddress(res.Result.Proposer),
		BorChainID: res.Result.BorChainID,
		Timestamp:  uint64(res.Result.Timestamp.GetSeconds()),
	}

	return milestone, nil
}

func (h *HeimdallGRPCClient) FetchLastNoAckMilestone(ctx context.Context) (string, error) {
	log.Info("Fetching milestone")

	res, err := h.client.FetchLastNoAckMilestone(ctx, nil)
	if err != nil {
		return "", err
	}

	log.Info("Fetched last no-ack milestone")

	return res.Result.Result, nil
}

func (h *HeimdallGRPCClient) FetchNoAckMilestone(ctx context.Context, milestoneID string) (bool, error) {
	req := &proto.FetchMilestoneNoAckRequest{
		MilestoneID: milestoneID,
	}

	log.Info("Fetching no ack milestone", "milestoneaID", milestoneID)

	res, err := h.client.FetchNoAckMilestone(ctx, req)
	if err != nil {
		return false, err
	}

	log.Info("Fetched no ack milestone", "milestoneaID", milestoneID)

	return res.Result.Result, nil
}
