package eth

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// interface for whitelist service
type ChainValidator interface {
	IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error)
	GetCheckpoints(current, sidechainHeader *types.Header, sidechainCheckpoints []*types.Header) (map[uint64]*types.Header, error)
	ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash)
	GetCheckpointWhitelist() map[uint64]common.Hash
	GetOldestCheckpoint() (uint64, common.Hash)
	PurgeCheckpointWhitelist()
}
