package heimdall

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/metrics"
)

type (
	requestTypeKey struct{}
	requestType    string

	meter struct {
		request map[bool]*metrics.Meter // map[isSuccessful]metrics.Meter
		timer   *metrics.Timer
	}
)

const (
	StateSyncRequest          requestType = "state-sync"
	SpanRequest               requestType = "span"
	LatestSpanRequest         requestType = "latest-span"
	CheckpointRequest         requestType = "checkpoint"
	CheckpointCountRequest    requestType = "checkpoint-count"
	MilestoneRequest          requestType = "milestone"
	MilestoneCountRequest     requestType = "milestone-count"
	MilestoneNoAckRequest     requestType = "milestone-no-ack"
	MilestoneLastNoAckRequest requestType = "milestone-last-no-ack"
	MilestoneIDRequest        requestType = "milestone-id"
)

func WithRequestType(ctx context.Context, reqType requestType) context.Context {
	return context.WithValue(ctx, requestTypeKey{}, reqType)
}

func getRequestType(ctx context.Context) (requestType, bool) {
	reqType, ok := ctx.Value(requestTypeKey{}).(requestType)
	return reqType, ok
}

var (
	requestMeters = map[requestType]meter{
		StateSyncRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/statesync/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/statesync/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/statesync/duration", nil),
		},
		SpanRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/span/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/span/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/span/duration", nil),
		},
		LatestSpanRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/latestspan/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/latestspan/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/latestspan/duration", nil),
		},
		CheckpointRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/checkpoint/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/checkpoint/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/checkpoint/duration", nil),
		},
		CheckpointCountRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/checkpointcount/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/checkpointcount/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/checkpointcount/duration", nil),
		},
		MilestoneRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/milestone/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/milestone/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/milestone/duration", nil),
		},
		MilestoneCountRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/milestonecount/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/milestonecount/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/milestonecount/duration", nil),
		},
		MilestoneNoAckRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/milestonenoack/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/milestonenoack/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/milestonenoack/duration", nil),
		},
		MilestoneLastNoAckRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/milestonelastnoack/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/milestonelastnoack/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/milestonelastnoack/duration", nil),
		},
		MilestoneIDRequest: {
			request: map[bool]*metrics.Meter{
				true:  metrics.NewRegisteredMeter("client/requests/milestoneid/valid", nil),
				false: metrics.NewRegisteredMeter("client/requests/milestoneid/invalid", nil),
			},
			timer: metrics.NewRegisteredTimer("client/requests/milestoneid/duration", nil),
		},
	}
)

func SendMetrics(ctx context.Context, start time.Time, isSuccessful bool) {
	reqType, ok := getRequestType(ctx)
	if !ok {
		return
	}

	meters, ok := requestMeters[reqType]
	if !ok {
		return
	}

	meters.request[isSuccessful].Mark(1)
	meters.timer.Update(time.Since(start))
}
