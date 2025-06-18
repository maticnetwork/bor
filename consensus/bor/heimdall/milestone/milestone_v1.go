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

type MilestoneLastNoAck struct {
	Result string `json:"result"`
}

type MilestoneLastNoAckResponse struct {
	Height string             `json:"height"`
	Result MilestoneLastNoAck `json:"result"`
}

type MilestoneNoAck struct {
	Result bool `json:"result"`
}

type MilestoneNoAckResponse struct {
	Height string         `json:"height"`
	Result MilestoneNoAck `json:"result"`
}

type MilestoneID struct {
	Result bool `json:"result"`
}

type MilestoneIDResponse struct {
	Height string      `json:"height"`
	Result MilestoneID `json:"result"`
}
