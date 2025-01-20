package checkpoint

import (
	"github.com/ethereum/go-ethereum/common"
)

// Checkpoint defines a response object type of bor checkpoint
type Checkpoint struct {
	Proposer   common.Address `json:"proposer"`
	StartBlock uint64         `json:"start_block"`
	EndBlock   uint64         `json:"end_block"`
	RootHash   common.Hash    `json:"root_hash"`
	BorChainID string         `json:"bor_chain_id"`
	Timestamp  uint64         `json:"timestamp"`
}

type CheckpointResponse struct {
	Result Checkpoint `json:"result"`
}

type CheckpointCountResponse struct {
	Result int64 `json:"result"`
}
