package bor

import (
	"context"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
)

//go:generate mockgen -source=heimdall.go -destination=../../tests/bor/mocks/IHeimdallClient.go -package=mocks
type IHeimdallClient interface {
	StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error)
	GetSpan(ctx context.Context, spanID uint64) (*types.Span, error)
	GetLatestSpan(ctx context.Context) (*types.Span, error)
	FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error)
	FetchCheckpointCount(ctx context.Context) (int64, error)
	FetchMilestone(ctx context.Context) (*milestone.Milestone, error)
	FetchMilestoneCount(ctx context.Context) (int64, error)
	Close()
}

//go:generate mockgen -destination=../../tests/bor/mocks/IHeimdallWSClient.go -package=mocks . IHeimdallWSClient
type IHeimdallWSClient interface {
	SubscribeMilestoneEvents(ctx context.Context) <-chan *milestone.Milestone
	Unsubscribe(ctx context.Context) error
	Close() error
}
