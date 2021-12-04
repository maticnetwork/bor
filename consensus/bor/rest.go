package bor

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

var (
	stateFetchLimit = 50
)

type IHeimdallClient interface {
	FetchStateSyncEvents(fromID uint64, to int64) ([]*EventRecordWithTime, error)
	FetchSpan(span uint64) (*HeimdallSpan, error)
}

type HeimdallClient struct {
	urlString string
	client    http.Client
}

func NewHeimdallClient(urlString string) (*HeimdallClient, error) {
	h := &HeimdallClient{
		urlString: urlString,
		client: http.Client{
			Timeout: time.Duration(5 * time.Second),
		},
	}
	return h, nil
}

func (h *HeimdallClient) FetchSpan(spanNum uint64) (*HeimdallSpan, error) {
	var span *HeimdallSpan
	response, err := h.fetchWithRetry(fmt.Sprintf("bor/span/%d", spanNum), "")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(response.Result, &span); err != nil {
		return nil, err
	}
	return span, nil
}

func (h *HeimdallClient) FetchStateSyncEvents(fromID uint64, to int64) ([]*EventRecordWithTime, error) {
	eventRecords := make([]*EventRecordWithTime, 0)
	for {
		queryParams := fmt.Sprintf("from-id=%d&to-time=%d&limit=%d", fromID, to, stateFetchLimit)
		log.Info("Fetching state sync events", "queryParams", queryParams)
		response, err := h.fetchWithRetry("clerk/event-record/list", queryParams)
		if err != nil {
			return nil, err
		}
		var _eventRecords []*EventRecordWithTime
		if response.Result == nil { // status 204
			break
		}
		if err := json.Unmarshal(response.Result, &_eventRecords); err != nil {
			return nil, err
		}
		eventRecords = append(eventRecords, _eventRecords...)
		if len(_eventRecords) < stateFetchLimit {
			break
		}
		fromID += uint64(stateFetchLimit)
	}

	sort.SliceStable(eventRecords, func(i, j int) bool {
		return eventRecords[i].ID < eventRecords[j].ID
	})
	return eventRecords, nil
}

// responseWithHeight defines a response object type that wraps an original
// response with a height.
type responseWithHeight struct {
	Height string          `json:"height"`
	Result json.RawMessage `json:"result"`
}

// FetchWithRetry returns data from heimdall with retry
func (h *HeimdallClient) fetchWithRetry(rawPath string, rawQuery string) (*responseWithHeight, error) {
	u, err := url.Parse(h.urlString)
	if err != nil {
		return nil, err
	}

	u.Path = rawPath
	u.RawQuery = rawQuery

	for {
		res, err := h.internalFetch(u)
		if err == nil && res != nil {
			return res, nil
		}
		log.Info("Retrying again in 5 seconds for next Heimdall span", "path", u.Path)
		time.Sleep(5 * time.Second)
	}
}

// internal fetch method
func (h *HeimdallClient) internalFetch(u *url.URL) (*responseWithHeight, error) {
	res, err := h.client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// check status code
	if res.StatusCode != 200 && res.StatusCode != 204 {
		return nil, fmt.Errorf("Error while fetching data from Heimdall")
	}

	// unmarshall data from buffer
	var response responseWithHeight
	if res.StatusCode == 204 {
		return &response, nil
	}

	// get response
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	return &response, nil
}
