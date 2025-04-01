package bor

import (
	"context"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
)

//go:generate mockgen -destination=../../tests/bor/mocks/IHeimdallClient.go -package=mocks . IHeimdallClient
type IHeimdallClient interface {
	StateSyncEventsV1(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error)
	StateSyncEventsV2(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error)
	GetSpanV1(ctx context.Context, spanID uint64) (*span.HeimdallSpan, error)
	GetSpanV2(ctx context.Context, spanID uint64) (*types.Span, error)
	GetLatestSpanV1(ctx context.Context) (*span.HeimdallSpan, error)
	GetLatestSpanV2(ctx context.Context) (*types.Span, error)
	FetchCheckpointV1(ctx context.Context, number int64) (*checkpoint.CheckpointV1, error)
	FetchCheckpointV2(ctx context.Context, number int64) (*checkpoint.CheckpointV2, error)
	FetchCheckpointCount(ctx context.Context) (int64, error)
	FetchMilestoneV1(ctx context.Context) (*milestone.MilestoneV1, error)
	FetchMilestoneV2(ctx context.Context) (*milestone.MilestoneV2, error)
	FetchMilestoneCount(ctx context.Context) (int64, error)
	FetchNoAckMilestone(ctx context.Context, milestoneID string) error // Fetch the bool value whether milestone corresponding to the given id failed in the Heimdall
	FetchLastNoAckMilestone(ctx context.Context) (string, error)       // Fetch latest failed milestone id
	Close()
}

//go:generate mockgen -destination=../../tests/bor/mocks/IHeimdallWSClient.go -package=mocks . IHeimdallWSClient
type IHeimdallWSClient interface {
	SubscribeMilestoneEvents(ctx context.Context) <-chan *milestone.MilestoneV2
	Unsubscribe(ctx context.Context) error
	Close() error
}
