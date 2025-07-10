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
	hmm "github.com/ethereum/go-ethereum/heimdall-migration-monitor"
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

type StateSyncEventsResponseV1 struct {
	Height string                       `json:"height"`
	Result []*clerk.EventRecordWithTime `json:"result"`
}

type SpanResponseV1 struct {
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
	fetchStateSyncEventsFormatV1 = "from-id=%d&to-time=%d&limit=%d"
	fetchStateSyncEventsPathV1   = "clerk/event-record/list"

	fetchLastNoAckMilestoneV1 = "/milestone/lastNoAck"
	fetchNoAckMilestoneV1     = "/milestone/noAck/%s"

	fetchStateSyncEventsFormatV2 = "from_id=%d&to_time=%s&pagination.limit=%d"
	fetchStateSyncEventsPathV2   = "clerk/time"

	fetchCheckpointV2      = "/checkpoints/%s"
	fetchCheckpointCountV2 = "/checkpoints/count"

	fetchV1Milestone      = "/milestone/latest"
	fetchV2Milestone      = "/milestones/latest"
	fetchMilestoneCountV1 = "/milestone/count"
	fetchMilestoneCountV2 = "/milestones/count"

	fetchSpanFormatV1 = "bor/span/%d"
	fetchSpanFormatV2 = "bor/spans/%d"
	fetchLatestSpanV1 = "bor/latest-span"
	fetchLatestSpanV2 = "bor/spans/latest"
)

// StateSyncEventsV1 fetches the state sync events from heimdall
func (h *HeimdallClient) StateSyncEventsV1(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	eventRecords := make([]*clerk.EventRecordWithTime, 0)

	for {
		url, err := stateSyncURLV1(h.urlString, fromID, to)
		if err != nil {
			return nil, err
		}

		log.Info("Fetching state sync events", "queryParams", url.RawQuery)

		ctx = WithRequestType(ctx, StateSyncRequest)

		request := &Request{client: h.client, url: url, start: time.Now()}
		response, err := Fetch[StateSyncEventsResponseV1](ctx, request)
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

// StateSyncEventsV2 fetches the state sync events from heimdall
func (h *HeimdallClient) StateSyncEventsV2(ctx context.Context, fromID uint64, to int64) ([]*clerk.EventRecordWithTime, error) {
	eventRecords := make([]*clerk.EventRecordWithTime, 0)

	for {
		url, err := stateSyncURLV2(h.urlString, fromID, to)
		if err != nil {
			return nil, err
		}

		log.Info("Fetching state sync events", "queryParams", url.RawQuery)

		ctx = WithRequestType(ctx, StateSyncRequest)

		request := &Request{client: h.client, url: url, start: time.Now()}
		response, err := Fetch[clerkTypes.RecordListResponse](ctx, request)
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

func (h *HeimdallClient) GetSpanV1(ctx context.Context, spanID uint64) (*span.HeimdallSpan, error) {
	url, err := spanV1URL(h.urlString, spanID)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, SpanRequest)

	request := &Request{client: h.client, url: url, start: time.Now()}
	response, err := Fetch[SpanResponseV1](ctx, request)
	if err != nil {
		return nil, err
	}
	return &response.Result, nil
}

func (h *HeimdallClient) GetSpanV2(ctx context.Context, spanID uint64) (*types.Span, error) {
	url, err := spanV2URL(h.urlString, spanID)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, SpanRequest)

	request := &Request{client: h.client, url: url, start: time.Now()}
	response, err := Fetch[types.QuerySpanByIdResponse](ctx, request)
	if err != nil {
		return nil, err
	}
	return response.Span, nil
}

func (h *HeimdallClient) GetLatestSpanV1(ctx context.Context) (*span.HeimdallSpan, error) {
	url, err := latestSpanUrlV1(h.urlString)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, SpanRequest)
	request := &Request{client: h.client, url: url, start: time.Now()}
	response, err := Fetch[SpanResponseV1](ctx, request)
	if err != nil {
		return nil, err
	}
	return &response.Result, nil
}

func (h *HeimdallClient) GetLatestSpanV2(ctx context.Context) (*types.Span, error) {
	url, err := latestSpanUrlV2(h.urlString)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, SpanRequest)
	request := &Request{client: h.client, url: url, start: time.Now()}
	response, err := Fetch[types.QueryLatestSpanResponse](ctx, request)
	if err != nil {
		return nil, err
	}
	return &response.Span, nil
}

// FetchCheckpointV1 fetches the checkpoint from heimdall
func (h *HeimdallClient) FetchCheckpointV1(ctx context.Context, number int64) (*checkpoint.CheckpointV1, error) {
	url, err := checkpointURL(h.urlString, number)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, CheckpointRequest)

	response, err := FetchWithRetry[checkpoint.CheckpointResponseV1](ctx, h.client, url, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchCheckpointV2 fetches the checkpoint from heimdall
func (h *HeimdallClient) FetchCheckpointV2(ctx context.Context, number int64) (*checkpoint.CheckpointV2, error) {
	url, err := checkpointURL(h.urlString, number)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, CheckpointRequest)

	response, err := FetchWithRetry[checkpoint.CheckpointResponseV2](ctx, h.client, url, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchMilestoneV1 fetches the checkpoint from heimdall
func (h *HeimdallClient) FetchMilestoneV1(ctx context.Context) (*milestone.MilestoneV1, error) {
	url, err := milestoneV1URL(h.urlString)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, MilestoneRequest)

	response, err := FetchWithRetry[milestone.MilestoneResponseV1](ctx, h.client, url, h.closeCh)
	if err != nil {
		return nil, err
	}

	return &response.Result, nil
}

// FetchMilestoneV2 fetches the milestone from heimdall
func (h *HeimdallClient) FetchMilestoneV2(ctx context.Context) (*milestone.MilestoneV2, error) {
	url, err := milestoneV2URL(h.urlString)
	if err != nil {
		return nil, err
	}

	ctx = WithRequestType(ctx, MilestoneRequest)

	response, err := FetchWithRetry[milestone.MilestoneResponseV2](ctx, h.client, url, h.closeCh)
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

	if hmm.IsHeimdallV2 {
		response, err := FetchWithRetry[checkpoint.CheckpointCountResponseV2](ctx, h.client, url, h.closeCh)
		if err != nil {
			return 0, err
		}
		return response.Result, nil
	}

	response, err := FetchWithRetry[checkpoint.CheckpointCountResponseV1](ctx, h.client, url, h.closeCh)
	if err != nil {
		return 0, err
	}
	return response.Result.Result, nil
}

// FetchMilestoneCount fetches the milestone count from heimdall
func (h *HeimdallClient) FetchMilestoneCount(ctx context.Context) (int64, error) {
	ctx = WithRequestType(ctx, MilestoneCountRequest)

	if hmm.IsHeimdallV2 {
		url, err := milestoneCountV2URL(h.urlString)
		if err != nil {
			return 0, err
		}

		response, err := FetchWithRetry[milestone.MilestoneCountResponseV2](ctx, h.client, url, h.closeCh)
		if err != nil {
			return 0, err
		}
		return response.Count, nil
	}

	url, err := milestoneCountV1URL(h.urlString)
	if err != nil {
		return 0, err
	}

	response, err := FetchWithRetry[milestone.MilestoneCountResponseV1](ctx, h.client, url, h.closeCh)
	if err != nil {
		return 0, err
	}
	return response.Result.Count, nil
}

// FetchLastNoAckMilestone fetches the last no-ack-milestone from heimdall
func (h *HeimdallClient) FetchLastNoAckMilestone(ctx context.Context) (string, error) {
	url, err := lastNoAckMilestoneURL(h.urlString)
	if err != nil {
		return "", err
	}

	ctx = WithRequestType(ctx, MilestoneLastNoAckRequest)

	response, err := FetchWithRetry[milestone.MilestoneLastNoAckResponse](ctx, h.client, url, h.closeCh)
	if err != nil {
		return "", err
	}

	return response.Result.Result, nil
}

// FetchNoAckMilestone fetches the last no-ack-milestone from heimdall
func (h *HeimdallClient) FetchNoAckMilestone(ctx context.Context, milestoneID string) error {
	url, err := noAckMilestoneURL(h.urlString, milestoneID)
	if err != nil {
		return err
	}

	ctx = WithRequestType(ctx, MilestoneNoAckRequest)

	response, err := FetchWithRetry[milestone.MilestoneNoAckResponse](ctx, h.client, url, h.closeCh)
	if err != nil {
		return err
	}

	if !response.Result.Result {
		return fmt.Errorf("%w: milestoneID %q", ErrNotInRejectedList, milestoneID)
	}

	return nil
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
		if metrics.Enabled() {
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

	if hmm.IsHeimdallV2 {
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
	}

	err = json.Unmarshal(body, result)
	if err != nil {
		return nil, err
	}

	isSuccessful = true

	return result, nil
}

func spanV1URL(urlString string, spanID uint64) (*url.URL, error) {
	return makeURL(urlString, fmt.Sprintf(fetchSpanFormatV1, spanID), "")
}

func spanV2URL(urlString string, spanID uint64) (*url.URL, error) {
	return makeURL(urlString, fmt.Sprintf(fetchSpanFormatV2, spanID), "")
}

func latestSpanUrlV1(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchLatestSpanV1, "")
}

func latestSpanUrlV2(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchLatestSpanV2, "")
}

func stateSyncURLV1(urlString string, fromID uint64, to int64) (*url.URL, error) {
	queryParams := fmt.Sprintf(fetchStateSyncEventsFormatV1, fromID, to, stateFetchLimit)

	return makeURL(urlString, fetchStateSyncEventsPathV1, queryParams)
}

func stateSyncURLV2(urlString string, fromID uint64, to int64) (*url.URL, error) {
	t := time.Unix(to, 0).UTC()
	formattedTime := t.Format(time.RFC3339Nano)

	queryParams := fmt.Sprintf(fetchStateSyncEventsFormatV2, fromID, formattedTime, stateFetchLimit)

	return makeURL(urlString, fetchStateSyncEventsPathV2, queryParams)
}

func checkpointURL(urlString string, number int64) (*url.URL, error) {
	url := ""
	if number == -1 {
		url = fmt.Sprintf(fetchCheckpointV2, "latest")
	} else {
		url = fmt.Sprintf(fetchCheckpointV2, fmt.Sprint(number))
	}

	return makeURL(urlString, url, "")
}

func milestoneV1URL(urlString string) (*url.URL, error) {
	url := fetchV1Milestone

	return makeURL(urlString, url, "")
}

func milestoneV2URL(urlString string) (*url.URL, error) {
	url := fetchV2Milestone

	return makeURL(urlString, url, "")
}

func checkpointCountURL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchCheckpointCountV2, "")
}

func milestoneCountV1URL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchMilestoneCountV1, "")
}

func milestoneCountV2URL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchMilestoneCountV2, "")
}

func lastNoAckMilestoneURL(urlString string) (*url.URL, error) {
	return makeURL(urlString, fetchLastNoAckMilestoneV1, "")
}

func noAckMilestoneURL(urlString string, id string) (*url.URL, error) {
	url := fmt.Sprintf(fetchNoAckMilestoneV1, id)
	return makeURL(urlString, url, "")
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
	if client.Timeout == 0 {
		// If no timeout is set, use a default timeout
		client.Timeout = 1 * time.Second
	}
	client.Timeout = 30 * time.Second
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
