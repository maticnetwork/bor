package bor

import (
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall"
)

type IHeimdallClient interface {
	Fetch(path string, query string) (*heimdall.ResponseWithHeight, error)
	FetchWithRetry(path string, query string) (*heimdall.ResponseWithHeight, error)
	FetchStateSyncEvents(fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error)
	Close()
}
