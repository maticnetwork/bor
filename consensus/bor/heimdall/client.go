package heimdall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"reflect"
	"sort"
	"time"

	"github.com/0xPolygon/heimdall-v2/x/bor/types"
	clerkTypes "github.com/0xPolygon/heimdall-v2/x/clerk/types"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/gogoproto/proto"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

var (
	// ErrShutdownDetected is returned if a shutdown was detected
	ErrShutdownDetected      = errors.New("shutdown detected")
	ErrNoResponse            = errors.New("got a nil response")
	ErrNotSuccessfulResponse = errors.New("error while fetching data from Heimdall")
	ErrNotInRejectedList     = errors.New("milestoneID doesn't exist in rejected list")
	ErrNotInMilestoneList    = errors.New("milestoneID doesn't exist in Heimdall")
	ErrServiceUnavailable    = errors.New("service unavailable")
)

const (
	heimdallAPIBodyLimit = 128 * 1024 * 1024 // 128 MB
	stateFetchLimit      = 50
	retryCall            = 5 * time.Second
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
	client http.Client
	url    *url.URL
	start  time.Time
}

func NewHeimdallClient(urlString string, timeout time.Duration) *HeimdallClient {
	return &HeimdallClient{
		urlString: urlString,
		client: http.Client{
			Timeout: timeout,
		},
		closeCh: make(chan struct{}),
	}
}

const (
	fetchStateSyncEventsFormat = "from-id=%d&to-time=%d"
	fetchStateSyncEventsPath   = "clerk/time"
	fetchStateSyncList         = "clerk/event-record/list"

	fetchCheckpoint      = "/checkpoints/%s"
	fetchCheckpointCount = "/checkpoints/count"

	fetchMilestone      = "/milestone/latest"
	fetchMilestoneCount = "/milestone/count"

	fetchSpanFormat = "bor/span/%d"
)

func (h *HeimdallClient) StateSyncEvents(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	eventRecords := make([]*clerk.EventRecordWithTime, 0)

	for {
		url, err := stateSyncListURL(h.urlString)
		if err != nil {
			return nil, err
		}

		log.Info("Fetching state sync events", "queryParams", url.RawQuery)

		ctx = WithRequestType(ctx, StateSyncRequest)

		response, err := FetchWithRetry[clerkTypes.RecordListResponse](ctx, h.client, url, h.closeCh)
		if err != nil {
			return nil, err
		}

		var record *clerk.EventRecordWithTime

		for _, e := range response.EventRecords {
			if e.Id >= fromID && e.RecordTime.Before(time.Unix(to, 0)) {
				record = &clerk.EventRecordWithTime{
					EventRecord: clerk.EventRecord{
						ID:       e.Id,
						ChainID:  e.BorChainId,
						Contract: common.HexToAddress(e.Contract),
						Data:     e.Data,
						LogIndex: e.LogIndex,
						TxHash:   common.HexToHash(e.TxHash),
					},
					Time: e.RecordTime,
				}
				eventRecords = append(eventRecords, record)
			}
		}

		if len(response.EventRecords) < stateFetchLimit {
			break
		}

		fromID += uint64(stateFetchLimit)
	}

	sort.SliceStable(eventRecords, func(i, j int) bool {
		return eventRecords[i].ID < eventRecords[j].ID
	})

	return eventRecords, nil
}

func (h *HeimdallClient) GetSpan(ctx context.Context, spanID uint64) (*types.Span, error) {
	url, err := spanURL(h.urlString, spanID)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, SpanRequest)

	response, err := FetchWithRetry[types.QuerySpanByIdResponse](ctx, h.client, url, h.closeCh)
	if err != nil {
		return nil, err
	}
	return response.Span, nil
}

// FetchCheckpoint fetches the checkpoint from heimdall
func (h *HeimdallClient) FetchCheckpoint(ctx context.Context, number int64) (*checkpoint.Checkpoint, error) {
	url, err := checkpointURL(h.urlString, number)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, CheckpointRequest)

	response, err := FetchWithRetry[checkpoint.CheckpointResponse](ctx, h.client, url, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchMilestone fetches the milestone from heimdall
func (h *HeimdallClient) FetchMilestone(ctx context.Context) (*milestone.Milestone, error) {
	url, err := milestoneURL(h.urlString)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, MilestoneRequest)

	response, err := FetchWithRetry[milestone.MilestoneResponse](ctx, h.client, url, h.closeCh)
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

	ctx = WithRequestType(ctx, CheckpointCountRequest)

	response, err := FetchWithRetry[checkpoint.CheckpointCountResponse](ctx, h.client, url, h.closeCh)
	if err != nil {
		return 0, err
	}

	return response.Result, nil
}

// FetchMilestoneCount fetches the milestone count from heimdall
func (h *HeimdallClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	url, err := milestoneCountURL(h.urlString)
	if err != nil {
		return 0, err
	}

	ctx = WithRequestType(ctx, MilestoneCountRequest)

	response, err := FetchWithRetry[milestone.MilestoneCountResponse](ctx, h.client, url, h.closeCh)
	if err != nil {
		return 0, err
	}

	return response.Count, nil
}

// FetchWithRetry returns data from heimdall with retry
func FetchWithRetry[T any](ctx context.Context, client http.Client, url *url.URL, closeCh chan struct{}) (*T, error) {
	// request data once
	request := &Request{client: client, url: url, start: time.Now()}
	result, err := Fetch[T](ctx, request)

	if err == nil {
		return result, nil
	}

	// 503 (Service Unavailable) is thrown when an endpoint isn't activated
	// yet in heimdall. E.g. when the hardfork hasn't hit yet but heimdall
	// is upgraded.
	if errors.Is(err, ErrServiceUnavailable) {
		log.Debug("Heimdall service unavailable at the moment", "path", url.Path, "error", err)
		return nil, err
	}

	// attempt counter
	attempt := 1

	log.Warn("an error while trying fetching from Heimdall", "path", url.Path, "attempt", attempt, "error", err)

	// create a new ticker for retrying the request
	var ticker *time.Ticker
	if client.Timeout != 0 {
		ticker = time.NewTicker(client.Timeout)
	} else {
		// only reach here when HeimdallClient is HeimdallGRPCClient or HeimdallAppClient
		ticker = time.NewTicker(retryCall)
	}
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
			request = &Request{client: client, url: url, start: time.Now()}
			result, err = Fetch[T](ctx, request)

			if errors.Is(err, ErrServiceUnavailable) {
				log.Debug("Heimdall service unavailable at the moment", "path", url.Path, "error", err)
				return nil, err
			}

			if err != nil {
				if attempt%logEach == 0 {
					log.Warn("an error while trying fetching from Heimdall", "path", url.Path, "attempt", attempt, "error", err)
				}

				continue retryLoop
			}

			return result, nil
		}
	}
}

// Fetch returns data from heimdall
func Fetch[T any](ctx context.Context, request *Request) (*T, error) {
	isSuccessful := false

	defer func() {
		if metrics.Enabled {
			SendMetrics(ctx, request.start, isSuccessful)
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

	p, ok := interface{}(result).(proto.Message)
	if ok {
		interfaceRegistry := codectypes.NewInterfaceRegistry()
		cryptocodec.RegisterInterfaces(interfaceRegistry)
		cdc := codec.NewProtoCodec(interfaceRegistry)

		err = cdc.UnmarshalJSON(body, p)
		if err != nil {
			return nil, err
		}

		tValue := reflect.ValueOf(result).Elem()
		tValue.Set(reflect.ValueOf(p).Elem())

		return result, nil
	}

	err = json.Unmarshal(body, result)
	if err != nil {
		return nil, err
	}

	isSuccessful = true

	return result, nil
}

func spanURL(urlString string, spanID uint64) (*url.URL, error) {
	return makeURL(urlString, fmt.Sprintf(fetchSpanFormat, spanID), "")
}

func stateSyncURL(urlString string, fromID uint64, to int64) (*url.URL, error) {
	queryParams := fmt.Sprintf(fetchStateSyncEventsFormat, fromID, to)

	return makeURL(urlString, fetchStateSyncEventsPath, queryParams)
}

func stateSyncListURL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchStateSyncList, "")
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

func milestoneURL(urlString string) (*url.URL, error) {
	url := fetchMilestone

	return makeURL(urlString, url, "")
}

func checkpointCountURL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchCheckpointCount, "")
}

func milestoneCountURL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchMilestoneCount, "")
}

func makeURL(urlString, rawPath, rawQuery string) (*url.URL, error) {
	u, err := url.Parse(urlString)
	if err != nil {
		return nil, err
	}

	u.Path = path.Join(u.Path, rawPath)
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

	if res.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("%w: response code %d", ErrServiceUnavailable, res.StatusCode)
	}

	// check status code
	if res.StatusCode != 200 && res.StatusCode != 204 {
		return nil, fmt.Errorf("%w: response code %d", ErrNotSuccessfulResponse, res.StatusCode)
	}

	// unmarshall data from buffer
	if res.StatusCode == 204 {
		return nil, nil
	}

	// Limit the number of bytes read from the response body
	limitedBody := http.MaxBytesReader(nil, res.Body, heimdallAPIBodyLimit)

	// get response
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func internalFetchWithTimeout(ctx context.Context, client http.Client, url *url.URL) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, client.Timeout)
	defer cancel()

	// request data once
	return internalFetch(ctx, client, url)
}

// Close sends a signal to stop the running process
func (h *HeimdallClient) Close() {
	close(h.closeCh)
	h.client.CloseIdleConnections()
}
