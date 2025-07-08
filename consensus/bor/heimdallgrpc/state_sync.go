package heimdallgrpc

import (
	"context"
	"time"

	"github.com/cosmos/cosmos-sdk/types/query"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall"
	"github.com/ethereum/go-ethereum/log"

	"github.com/0xPolygon/heimdall-v2/x/clerk/types"
)

func (h *HeimdallGRPCClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	log.Info("Fetching state sync events", "fromID", fromID, "to", to)

	var err error

	ctxWithTimeout, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	// Start the timer and set the request type on the context.
	start := time.Now()
	ctx = heimdall.WithRequestType(ctxWithTimeout, heimdall.StateSyncRequest)

	// Defer the metrics call.
	defer func() {
		heimdall.SendMetrics(ctx, start, err == nil)
	}()

	eventRecords := make([]*clerk.EventRecordWithTime, 0)

	pagination := query.PageRequest{
		Limit: stateFetchLimit,
	}

	req := &types.RecordListWithTimeRequest{
		FromId:     fromID,
		ToTime:     time.Unix(to, 0),
		Pagination: pagination,
	}

	res, err := h.clerkQueryClient.GetRecordListWithTime(ctx, req)
	if err != nil {
		return nil, err
	}

	events := res.GetEventRecords()

	for _, event := range events {
		eventRecord := &clerk.EventRecordWithTime{
			EventRecord: clerk.EventRecord{
				ID:       event.Id,
				Contract: common.HexToAddress(event.Contract),
				Data:     event.Data,
				TxHash:   common.HexToHash(event.TxHash),
				LogIndex: event.LogIndex,
				ChainID:  event.BorChainId,
			},
			Time: event.RecordTime,
		}
		eventRecords = append(eventRecords, eventRecord)
	}

	log.Info("Fetched state sync events", "fromID", fromID, "to", to)

	return eventRecords, nil
}
