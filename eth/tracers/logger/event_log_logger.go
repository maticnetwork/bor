package logger

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)


type EventLogLogger struct {
	eventsLogs []*types.LogWithAddress
// contractAddress to array of emited log
}

func NewEventLogLogger() *EventLogLogger {
	return &EventLogLogger{
		eventsLogs: make([]*types.LogWithAddress, 0),
	}
}

func (logger *EventLogLogger) CaptureEventLogs(address common.Address, logs []*types.Log) {
	logger.eventsLogs = append(logger.eventsLogs, &types.LogWithAddress{
		Address: address,
		Logs: logs,
	})
}

func (logger *EventLogLogger) GetEventsLogsMap() []*types.LogWithAddress{
	return logger.eventsLogs
}
