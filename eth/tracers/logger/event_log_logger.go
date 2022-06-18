package logger

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type EventLogLogger struct {
	EventsLogs map[common.Address][]*types.Log // contractAddress to array of emited log
}

func (logger *EventLogLogger)  CaptureEventLogs(address common.Address, logs []*types.Log) {
	array, in := logger.EventsLogs[address]
	fmt.Printf("Receive %d logs from %s", len(logs), address.String())
	if !in {
    	logger.EventsLogs[address] = logs
    	return
	}

	logger.EventsLogs[address] = append(array, logs...)
}
