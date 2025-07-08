package checkpoint

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

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

func (m *Checkpoint) UnmarshalJSON(data []byte) error {
	type Alias Checkpoint
	temp := &struct {
		StartBlock string `json:"start_block"`
		EndBlock   string `json:"end_block"`
		RootHash   string `json:"root_hash"`
		Timestamp  string `json:"timestamp"`
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

	decodedHash, err := base64.StdEncoding.DecodeString(temp.RootHash)
	if err != nil {
		return fmt.Errorf("failed to decode hash: %w", err)
	}
	m.RootHash = common.BytesToHash(decodedHash)

	timestamp, err := strconv.ParseUint(temp.Timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	m.Timestamp = timestamp

	return nil
}

type CheckpointResponse struct {
	Result Checkpoint `json:"checkpoint"`
}

func (m *CheckpointCountResponse) UnmarshalJSON(data []byte) error {
	temp := &struct {
		Count string `json:"ack_count"`
	}{}

	if err := json.Unmarshal(data, temp); err != nil {
		return err
	}

	count, err := strconv.ParseInt(temp.Count, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid count: %w", err)
	}
	m.Result = count

	return nil
}

type CheckpointCountResponse struct {
	Result int64 `json:"ack_count"`
}
