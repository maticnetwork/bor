package bor

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// EventRecord represents state record
type EventRecordWithTime struct {
	ID       uint64         `json:"id,string" yaml:"id"`
	Contract common.Address `json:"contract,string" yaml:"contract"`
	Data     []byte  `json:"data" yaml:"data"`
	TxHash   common.Hash    `json:"tx_hash,string" yaml:"tx_hash"`
	LogIndex uint64         `json:"log_index,string" yaml:"log_index"`
	ChainID  string         `json:"chain_id" yaml:"chain_id"`
	Time     time.Time      `json:"record_time,string" yaml:"record_time"`
}

// type EventRecordWithTime struct {
// 	EventRecord
// }

type EventRecordListResponse struct {
	EventRecordList []EventRecordWithTime `json:"event_records"`
}

// String returns the string representatin of span
func (e *EventRecordWithTime) String() string {
	return fmt.Sprintf(
		"id %v, contract %v, data: %v, txHash: %v, logIndex: %v, chainId: %v, time %s",
		e.ID,
		e.Contract.String(),
		e.Data,
		e.TxHash.Hex(),
		e.LogIndex,
		e.ChainID,
		e.Time.Format(time.RFC3339),
	)
}

func (e *EventRecordWithTime) BuildEventRecord() *EventRecordWithTime {
	return &EventRecordWithTime{
		ID:       e.ID,
		Contract: e.Contract,
		Data:     e.Data,
		TxHash:   e.TxHash,
		LogIndex: e.LogIndex,
		ChainID:  e.ChainID,
	}
}
