package checkpoint

import (
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
)

// CheckpointV2 defines a response object type of bor checkpoint
type CheckpointV2 struct {
	Proposer   common.Address `json:"proposer"`
	StartBlock uint64         `json:"start_block"`
	EndBlock   uint64         `json:"end_block"`
	RootHash   common.Hash    `json:"root_hash"`
	BorChainID string         `json:"bor_chain_id"`
	Timestamp  uint64         `json:"timestamp"`
}

func (m *CheckpointV2) UnmarshalJSON(data []byte) error {
	// Define a temp struct that matches the JSON structure.
	var temp struct {
		Proposer   string `json:"proposer"`
		StartBlock uint64 `json:"start_block"`
		EndBlock   uint64 `json:"end_block"`
		RootHash   string `json:"root_hash"`
		BorChainID string `json:"bor_chain_id"`
		Timestamp  uint64 `json:"timestamp"`
	}

	// Unmarshal the JSON into the temp struct.
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}

	m.Proposer = common.HexToAddress(temp.Proposer)
	m.StartBlock = temp.StartBlock
	m.EndBlock = temp.EndBlock
	m.RootHash = common.HexToHash(temp.RootHash)
	m.BorChainID = temp.BorChainID
	m.Timestamp = temp.Timestamp

	return nil
}

type CheckpointResponseV2 struct {
	Result CheckpointV2 `json:"checkpoint"`
}

type CheckpointCountResponseV2 struct {
	Result int64 `json:"ack_count"`
}
