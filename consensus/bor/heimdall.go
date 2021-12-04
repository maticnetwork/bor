package bor

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type HeimdallClient interface {
	FetchStateSyncEvents(fromID uint64, to int64) ([]*EventRecordWithTime, error)
	FetchSpan(span uint64) (*HeimdallSpan, error)
}

// EventRecord represents a Heimdall state record
type EventRecord struct {
	ID       uint64         `json:"id" yaml:"id"`
	Contract common.Address `json:"contract" yaml:"contract"`
	Data     hexutil.Bytes  `json:"data" yaml:"data"`
	TxHash   common.Hash    `json:"tx_hash" yaml:"tx_hash"`
	LogIndex uint64         `json:"log_index" yaml:"log_index"`
	ChainID  string         `json:"bor_chain_id" yaml:"bor_chain_id"`
}

// EventRecordWithTime represents a Heimdall state record that includes the timestamp
type EventRecordWithTime struct {
	EventRecord

	Time time.Time `json:"record_time" yaml:"record_time"`
}

// String returns the string representatin of span
func (e *EventRecordWithTime) String() string {
	return fmt.Sprintf(
		"id %v, contract %v, data: %v, txHash: %v, logIndex: %v, chainId: %v, time %s",
		e.ID,
		e.Contract.String(),
		e.Data.String(),
		e.TxHash.Hex(),
		e.LogIndex,
		e.ChainID,
		e.Time.Format(time.RFC3339),
	)
}

func (e *EventRecordWithTime) BuildEventRecord() *EventRecord {
	return &EventRecord{
		ID:       e.ID,
		Contract: e.Contract,
		Data:     e.Data,
		TxHash:   e.TxHash,
		LogIndex: e.LogIndex,
		ChainID:  e.ChainID,
	}
}

// Span represents a current bor span
type Span struct {
	ID         uint64 `json:"span_id" yaml:"span_id"`
	StartBlock uint64 `json:"start_block" yaml:"start_block"`
	EndBlock   uint64 `json:"end_block" yaml:"end_block"`
}

// HeimdallSpan represents span from heimdall APIs
type HeimdallSpan struct {
	Span
	ValidatorSet      ValidatorSet `json:"validator_set" yaml:"validator_set"`
	SelectedProducers []Validator  `json:"selected_producers" yaml:"selected_producers"`
	ChainID           string       `json:"bor_chain_id" yaml:"bor_chain_id"`
}
