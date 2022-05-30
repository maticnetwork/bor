package heimdall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"
)

// errShutdownDetected is returned if a shutdown was detected
var errShutdownDetected = errors.New("shutdown detected")

const (
	stateFetchLimit    = 50
	apiHeimdallTimeout = 5 * time.Second
)

// ResponseWithHeight defines a response object type that wraps an original
// response with a height.
// FIXME: Remove!!!
type ResponseWithHeight struct {
	Height string          `json:"height"`
	Result json.RawMessage `json:"result"`
}

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

	fetchSpanFormat = "%d"
	fetchSpanPath   = "bor/span/"
)

func (h *HeimdallClient) StateSyncEvents(fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	eventRecords := make([]*clerk.EventRecordWithTime, 0)

	for {
		queryParams := fmt.Sprintf(fetchStateSyncEventsFormat, fromID, to, stateFetchLimit)

		log.Info("Fetching state sync events", "queryParams", queryParams)

		response, err := FetchWithRetry[StateSyncEventsResponse](h.client, h.urlString, fetchStateSyncEventsPath, queryParams, h.closeCh)
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

func (h *HeimdallClient) Span(spanID uint64) (*span.HeimdallSpan, error) {
	queryParams := fmt.Sprintf(fetchSpanFormat, spanID)

	response, err := FetchWithRetry[SpanResponse](h.client, h.urlString, fetchSpanPath, queryParams, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchWithRetry returns data from heimdall with retry
func FetchWithRetry[T any](client http.Client, urlString string, rawPath string, rawQuery string, closeCh chan struct{}) (*T, error) {
	u, err := url.Parse(urlString)
	if err != nil {
		return nil, err
	}

	u.Path = rawPath
	u.RawQuery = rawQuery

	// attempt counter
	attempt := 1
	result := new(T)

	ctx, cancel := context.WithTimeout(context.Background(), apiHeimdallTimeout)

	// request data once
	body, err := internalFetch(ctx, client, u)

	cancel()

	if err == nil && body != nil {
		err = json.Unmarshal(body, result)
		if err != nil {
			return nil, err
		}

		return result, nil
	}

	// create a new ticker for retrying the request
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		log.Info("Retrying again in 5 seconds to fetch data from Heimdall", "path", u.Path, "attempt", attempt)
		attempt++
		select {
		case <-closeCh:
			log.Debug("Shutdown detected, terminating request")

			return nil, errShutdownDetected
		case <-ticker.C:
			ctx, cancel = context.WithTimeout(context.Background(), apiHeimdallTimeout)

			body, err = internalFetch(ctx, client, u)

			cancel()

			if err == nil && body != nil {
				err = json.Unmarshal(body, result)
				if err != nil {
					return nil, err
				}

				return result, nil
			}
		}
	}
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
		return nil, fmt.Errorf("Error while fetching data from Heimdall")
	}

	// unmarshall data from buffer
	if res.StatusCode == 204 {
		return nil, nil
	}

	// get response
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

// Close sends a signal to stop the running process
func (h *HeimdallClient) Close() {
	close(h.closeCh)
	h.client.CloseIdleConnections()
}
