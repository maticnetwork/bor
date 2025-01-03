package milestone

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
)

// milestone defines a response object type of bor milestone
type Milestone struct {
	Proposer    common.Address `json:"proposer"`
	StartBlock  uint64         `json:"start_block"`
	EndBlock    uint64         `json:"end_block"`
	Hash        common.Hash    `json:"hash"`
	BorChainID  string         `json:"bor_chain_id"`
	MilestoneID string         `json:"milestone_id"`
	Timestamp   uint64         `json:"timestamp"`
}

func (m *Milestone) UnmarshalJSON(data []byte) error {
	type Alias Milestone
	temp := &struct {
		StartBlock string `json:"start_block"`
		EndBlock   string `json:"end_block"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, temp); err != nil {
		return err
	}

	startBlock, err := strconv.ParseUint(temp.StartBlock, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid start_block: %w", err)
	}
	m.StartBlock = startBlock

	endBlock, err := strconv.ParseUint(temp.EndBlock, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid end_block: %w", err)
	}
	m.EndBlock = endBlock

	return nil
}

type MilestoneResponse struct {
	Result Milestone `json:"milestone"`
}

type MilestoneCount struct {
	Count int64 `json:"count"`
}

type MilestoneCountResponse struct {
	Height string         `json:"height"`
	Result MilestoneCount `json:"result"`
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
