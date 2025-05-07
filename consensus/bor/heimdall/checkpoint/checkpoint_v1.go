package checkpoint

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// CheckpointV1 defines a response object type of bor checkpoint
type CheckpointV1 struct {
	Proposer   common.Address `json:"proposer"`
	StartBlock *big.Int       `json:"start_block"`
	EndBlock   *big.Int       `json:"end_block"`
	RootHash   common.Hash    `json:"root_hash"`
	BorChainID string         `json:"bor_chain_id"`
	Timestamp  uint64         `json:"timestamp"`
}

type CheckpointResponseV1 struct {
	Height string       `json:"height"`
	Result CheckpointV1 `json:"result"`
}

type CheckpointCountV1 struct {
	Result int64 `json:"result"`
}

type CheckpointCountResponseV1 struct {
	Height string            `json:"height"`
	Result CheckpointCountV1 `json:"result"`
}
