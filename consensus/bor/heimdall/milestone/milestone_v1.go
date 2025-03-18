package milestone

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// MilestoneV1 defines a response object type of bor milestone
type MilestoneV1 struct {
	Proposer   common.Address `json:"proposer"`
	StartBlock *big.Int       `json:"start_block"`
	EndBlock   *big.Int       `json:"end_block"`
	Hash       common.Hash    `json:"hash"`
	BorChainID string         `json:"bor_chain_id"`
	Timestamp  uint64         `json:"timestamp"`
}

type MilestoneResponseV1 struct {
	Height string      `json:"height"`
	Result MilestoneV1 `json:"result"`
}

type MilestoneCountV1 struct {
	Count int64 `json:"count"`
}

type MilestoneCountResponseV1 struct {
	Height string           `json:"height"`
	Result MilestoneCountV1 `json:"result"`
}

type MilestoneLastNoAckV1 struct {
	Result string `json:"result"`
}

type MilestoneLastNoAckResponseV1 struct {
	Height string               `json:"height"`
	Result MilestoneLastNoAckV1 `json:"result"`
}

type MilestoneNoAckV1 struct {
	Result bool `json:"result"`
}

type MilestoneNoAckResponseV1 struct {
	Height string           `json:"height"`
	Result MilestoneNoAckV1 `json:"result"`
}

type MilestoneIDV1 struct {
	Result bool `json:"result"`
}

type MilestoneIDResponseV1 struct {
	Height string        `json:"height"`
	Result MilestoneIDV1 `json:"result"`
}
