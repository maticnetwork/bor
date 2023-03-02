package heimdallapp

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"

	jsoniter "github.com/json-iterator/go"
	abci "github.com/tendermint/tendermint/abci/types"
)

func (h *HeimdallAppClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	totalRecords := make([]*clerk.EventRecordWithTime, 0)

	hCtx := h.hApp.NewContext(true, abci.Header{Height: h.hApp.LastBlockHeight()})

	for {
		fromRecord, err := h.hApp.ClerkKeeper.GetEventRecord(hCtx, fromID)
		if err != nil {
			return nil, err
		}

		events, err := h.hApp.ClerkKeeper.GetEventRecordListWithTime(hCtx, fromRecord.RecordTime, time.Unix(to, 0), 1, stateFetchLimit)
		if err != nil {
			return nil, err
		}

		eventsByte, err := jsoniter.ConfigFastest.Marshal(events)
		if err != nil {
			return nil, err
		}

		var eventRecords []*clerk.EventRecordWithTime

		err = jsoniter.ConfigFastest.Unmarshal(eventsByte, &eventRecords)
		if err != nil {
			return nil, err
		}

		totalRecords = append(totalRecords, eventRecords...)

		if len(events) < stateFetchLimit {
			break
		}

		fromID += uint64(stateFetchLimit)
	}

	return totalRecords, nil
}
