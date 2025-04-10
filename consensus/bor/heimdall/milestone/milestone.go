package milestone

import (
	"encoding/json"

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
	// Define a temp struct that matches the JSON structure.
	var temp struct {
		Proposer    string `json:"proposer"`
		StartBlock  uint64 `json:"start_block"`
		EndBlock    uint64 `json:"end_block"`
		Hash        string `json:"hash"`
		BorChainID  string `json:"bor_chain_id"`
		MilestoneID string `json:"milestone_id"`
		Timestamp   uint64 `json:"timestamp"`
	}

	// Unmarshal the JSON into the temp struct.
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}

	m.Proposer = common.HexToAddress(temp.Proposer)
	m.StartBlock = temp.StartBlock
	m.EndBlock = temp.EndBlock
	m.Hash = common.HexToHash(temp.Hash)
	m.BorChainID = temp.BorChainID
	m.MilestoneID = temp.MilestoneID
	m.Timestamp = temp.Timestamp

	return nil
}

type MilestoneResponse struct {
	Result Milestone `json:"milestone"`
}

type MilestoneCountResponse struct {
	Count int64 `json:"count"`
}
