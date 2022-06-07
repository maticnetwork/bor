package eth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// interface for whitelist service
type ChainValidator interface {
	IsValidChain(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error)
	ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash)
	GetCheckpointWhitelist() map[uint64]common.Hash
	PurgeCheckpointWhitelist()
}
