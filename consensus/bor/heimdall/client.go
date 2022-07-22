package heimdall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

var (
	// ErrShutdownDetected is returned if a shutdown was detected
	ErrShutdownDetected      = errors.New("shutdown detected")
	ErrNoResponse            = errors.New("got a nil response")
	ErrNotSuccessfulResponse = errors.New("error while fetching data from Heimdall")
)

const (
	stateFetchLimit    = 50
	apiHeimdallTimeout = 5 * time.Second
	retryCall          = 5 * time.Second
)

type RequestType string

const (
	StateSyncRequest       RequestType = "state-sync"
	SpanRequest            RequestType = "span"
	CheckpointRequest      RequestType = "checkpoint"
	CheckpointCountRequest RequestType = "checkpoint-count"
)

type StateSyncEventsResponse struct {
	Height string                       `json:"height"`
	Result []*clerk.EventRecordWithTime `json:"result"`
}

type SpanResponse struct {
	Height string            `json:"height"`
	Result span.HeimdallSpan `json:"result"`
}

type HeimdallClient struct {
	urlString string
	client    http.Client
	closeCh   chan struct{}
}

type Request struct {
	client  http.Client
	url     *url.URL
	start   time.Time
	reqType RequestType
}

var (
	// State Sync requests
	stateSyncValidRequestMeter   = metrics.NewRegisteredMeter("client/requests/statesync/valid", nil)
	stateSyncInvalidRequestMeter = metrics.NewRegisteredMeter("client/requests/statesync/invalid", nil)
	stateSyncRequestTimer        = metrics.NewRegisteredTimer("client/requests/statesync/duration", nil)

	// Span requests
	spanValidRequestMeter   = metrics.NewRegisteredMeter("client/requests/span/valid", nil)
	spanInvalidRequestMeter = metrics.NewRegisteredMeter("client/requests/span/invalid", nil)
	spanRequestTimer        = metrics.NewRegisteredTimer("client/requests/span/duration", nil)

	// Checkpoint requests
	checkpointValidRequestMeter   = metrics.NewRegisteredMeter("client/requests/checkpoint/valid", nil)
	checkpointInvalidRequestMeter = metrics.NewRegisteredMeter("client/requests/checkpoint/invalid", nil)
	checkpointRequestTimer        = metrics.NewRegisteredTimer("client/requests/checkpoint/duration", nil)

	// Checkpoint count requests
	checkpointCountValidRequestMeter   = metrics.NewRegisteredMeter("client/requests/checkpointcount/valid", nil)
	checkpointCountInvalidRequestMeter = metrics.NewRegisteredMeter("client/requests/checkpointcount/invalid", nil)
	checkpointCountRequestTimer        = metrics.NewRegisteredTimer("client/requests/checkpointcount/duration", nil)
)

func NewHeimdallClient(urlString string) *HeimdallClient {
	return &HeimdallClient{
		urlString: urlString,
		client: http.Client{
			Timeout: apiHeimdallTimeout,
		},
		closeCh: make(chan struct{}),
	}
}

const (
	fetchStateSyncEventsFormat = "from-id=%d&to-time=%d&limit=%d"
	fetchStateSyncEventsPath   = "clerk/event-record/list"
	fetchCheckpoint            = "/checkpoints/%s"
	fetchCheckpointCount       = "/checkpoints/count"

	fetchSpanFormat = "bor/span/%d"
)

func (h *HeimdallClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	eventRecords := make([]*clerk.EventRecordWithTime, 0)

	for {
		url, err := stateSyncURL(h.urlString, fromID, to)
		if err != nil {
			return nil, err
		}

		log.Info("Fetching state sync events", "queryParams", url.RawQuery)

		response, err := FetchWithRetry[StateSyncEventsResponse](ctx, h.client, url, StateSyncRequest, h.closeCh)
		if err != nil {
			return nil, err
		}

		if response == nil || response.Result == nil {
			// status 204
			break
		}

		eventRecords = append(eventRecords, response.Result...)

		if len(response.Result) < stateFetchLimit {
			break
		}

		fromID += uint64(stateFetchLimit)
	}

	sort.SliceStable(eventRecords, func(i, j int) bool {
		return eventRecords[i].ID < eventRecords[j].ID
	})

	return eventRecords, nil
}

func (h *HeimdallClient) Span(ctx context.Context, spanID uint64) (*span.HeimdallSpan, error) {
	url, err := spanURL(h.urlString, spanID)
	if err != nil {
		return nil, err
	}

	response, err := FetchWithRetry[SpanResponse](ctx, h.client, url, SpanRequest, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchCheckpoint fetches the checkpoint from heimdall
func (h *HeimdallClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	url, err := checkpointURL(h.urlString, number)
	if err != nil {
		return nil, err
	}

	response, err := FetchWithRetry[checkpoint.CheckpointResponse](ctx, h.client, url, CheckpointRequest, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchCheckpointCount fetches the checkpoint count from heimdall
func (h *HeimdallClient) FetchCheckpointCount(ctx context.Context) (int64, error) {
	url, err := checkpointCountURL(h.urlString)
	if err != nil {
		return 0, err
	}

	response, err := FetchWithRetry[checkpoint.CheckpointCountResponse](ctx, h.client, url, CheckpointCountRequest, h.closeCh)
	if err != nil {
		return 0, err
	}

	return response.Result.Result, nil
}

// FetchWithRetry returns data from heimdall with retry
func FetchWithRetry[T any](ctx context.Context, client http.Client, url *url.URL, reqType RequestType, closeCh chan struct{}) (*T, error) {
	// request data once
	request := &Request{client: client, url: url, start: time.Now(), reqType: reqType}
	result, err := Fetch[T](ctx, request)

	if err == nil {
		return result, nil
	}

	// attempt counter
	attempt := 1

	log.Warn("an error while trying fetching from Heimdall", "attempt", attempt, "error", err)

	// create a new ticker for retrying the request
	ticker := time.NewTicker(retryCall)
	defer ticker.Stop()

	const logEach = 5

retryLoop:
	for {
		log.Info("Retrying again in 5 seconds to fetch data from Heimdall", "path", url.Path, "attempt", attempt)

		attempt++

		select {
		case <-ctx.Done():
			log.Debug("Shutdown detected, terminating request by context.Done")

			return nil, ctx.Err()
		case <-closeCh:
			log.Debug("Shutdown detected, terminating request by closing")

			return nil, ErrShutdownDetected
		case <-ticker.C:
			request := &Request{client: client, url: url, start: time.Now()}
			result, err = Fetch[T](ctx, request)

			if err != nil {
				if attempt%logEach == 0 {
					log.Warn("an error while trying fetching from Heimdall", "attempt", attempt, "error", err)
				}

				continue retryLoop
			}

			return result, nil
		}
	}
}

// Fetch returns data from heimdall
func Fetch[T any](ctx context.Context, request *Request) (*T, error) {
	var failed bool = true

	defer func() {
		if metrics.EnabledExpensive {
			sendMetrics(request, failed)
		}
	}()

	result := new(T)

	body, err := internalFetchWithTimeout(ctx, request.client, request.url)
	if err != nil {
		return nil, err
	}

	if body == nil {
		return nil, ErrNoResponse
	}

	err = json.Unmarshal(body, result)
	if err != nil {
		return nil, err
	}

	failed = false

	return result, nil
}

func spanURL(urlString string, spanID uint64) (*url.URL, error) {
	return makeURL(urlString, fmt.Sprintf(fetchSpanFormat, spanID), "")
}

func stateSyncURL(urlString string, fromID uint64, to int64) (*url.URL, error) {
	queryParams := fmt.Sprintf(fetchStateSyncEventsFormat, fromID, to, stateFetchLimit)

	return makeURL(urlString, fetchStateSyncEventsPath, queryParams)
}

func checkpointURL(urlString string, number int64) (*url.URL, error) {
	url := ""
	if number == -1 {
		url = fmt.Sprintf(fetchCheckpoint, "latest")
	} else {
		url = fmt.Sprintf(fetchCheckpoint, fmt.Sprint(number))
	}

	return makeURL(urlString, url, "")
}

func checkpointCountURL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchCheckpointCount, "")
}

func makeURL(urlString, rawPath, rawQuery string) (*url.URL, error) {
	u, err := url.Parse(urlString)
	if err != nil {
		return nil, err
	}

	u.Path = rawPath
	u.RawQuery = rawQuery

	return u, err
}

// internal fetch method
func internalFetch(ctx context.Context, client http.Client, u *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()

	// check status code
	if res.StatusCode != 200 && res.StatusCode != 204 {
		return nil, fmt.Errorf("%w: response code %d", ErrNotSuccessfulResponse, res.StatusCode)
	}

	// unmarshall data from buffer
	if res.StatusCode == 204 {
		return nil, nil
	}

	// get response
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func internalFetchWithTimeout(ctx context.Context, client http.Client, url *url.URL) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, apiHeimdallTimeout)
	defer cancel()

	// request data once
	return internalFetch(ctx, client, url)
}

// Close sends a signal to stop the running process
func (h *HeimdallClient) Close() {
	close(h.closeCh)
	h.client.CloseIdleConnections()
}

func sendMetrics(request *Request, failed bool) {
	if failed {
		switch request.reqType {
		case StateSyncRequest:
			stateSyncInvalidRequestMeter.Mark(1)
			stateSyncRequestTimer.Update(time.Since(request.start))
		case SpanRequest:
			spanInvalidRequestMeter.Mark(1)
			spanRequestTimer.Update(time.Since(request.start))
		case CheckpointRequest:
			checkpointInvalidRequestMeter.Mark(1)
			checkpointRequestTimer.Update(time.Since(request.start))
		case CheckpointCountRequest:
			checkpointCountInvalidRequestMeter.Mark(1)
			checkpointCountRequestTimer.Update(time.Since(request.start))
		}
	} else {
		switch request.reqType {
		case StateSyncRequest:
			stateSyncValidRequestMeter.Mark(1)
			stateSyncRequestTimer.Update(time.Since(request.start))
		case SpanRequest:
			spanValidRequestMeter.Mark(1)
			spanRequestTimer.Update(time.Since(request.start))
		case CheckpointRequest:
			checkpointValidRequestMeter.Mark(1)
			checkpointRequestTimer.Update(time.Since(request.start))
		case CheckpointCountRequest:
			checkpointCountValidRequestMeter.Mark(1)
			checkpointCountRequestTimer.Update(time.Since(request.start))
		}
	}
}
