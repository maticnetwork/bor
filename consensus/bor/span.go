package bor

import (
	"context"

	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

//go:generate mockgen -destination=./span_mock.go -package=bor . Spanner
type Spanner interface {
	GetCurrentSpan(ctx context.Context, headerHash common.Hash, state *state.StateDB) (*span.Span, error)
	GetCurrentValidatorsByHash(ctx context.Context, headerHash common.Hash, blockNumber uint64) ([]*valset.Validator, error)
	GetCurrentValidatorsByBlockNrOrHash(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash, blockNumber uint64) ([]*valset.Validator, error)
	CommitSpan(ctx context.Context, minimalSpan span.Span, validators, producers []stakeTypes.MinimalVal, state *state.StateDB, header *types.Header, chainContext core.ChainContext) error
	// CommitSpanV2(ctx context.Context, heimdallSpan borTypes.Span, state *state.StateDB, header *types.Header, chainContext core.ChainContext) error
}
