package heimdallws

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/log"
	"github.com/gorilla/websocket"
)

// HeimdallWSClient represents a websocket client with auto-reconnection.
type HeimdallWSClient struct {
	conn   *websocket.Conn
	url    string // store the URL for reconnection
	events chan *milestone.Milestone
	done   chan struct{}
	mu     sync.Mutex
}

// NewHeimdallWSClient creates a new WS client for Heimdall.
func NewHeimdallWSClient(url string) (*HeimdallWSClient, error) {
	return &HeimdallWSClient{
		conn:   nil,
		url:    url,
		events: make(chan *milestone.Milestone),
		done:   make(chan struct{}),
	}, nil
}

// SubscribeMilestoneEvents sends the subscription request and starts processing incoming messages.
func (c *HeimdallWSClient) SubscribeMilestoneEvents(ctx context.Context) <-chan *milestone.Milestone {
	c.tryUntilSubscribeMilestoneEvents(ctx)

	// Start the goroutine to read messages.
	go c.readMessages(ctx)

	return c.events
}

// retry until subscribe
func (c *HeimdallWSClient) tryUntilSubscribeMilestoneEvents(ctx context.Context) {
	firstTime := true
	for {
		if !firstTime {
			time.Sleep(10 * time.Second)
		}
		firstTime = false

		// Check for context cancellation.
		select {
		case <-ctx.Done():
			log.Info("Context cancelled during reconnection")
			return
		case <-c.done:
			log.Info("Client unsubscribed during reconnection")
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
		if err != nil {
			log.Error("failed to dial websocket on heimdall ws subscription", "err", err)
			continue
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()

		// Build the subscription request.
		req := subscriptionRequest{
			JSONRPC: "2.0",
			Method:  "subscribe",
			ID:      0,
		}
		req.Params.Query = "tm.event='NewBlock' AND milestone.number>0"

		if err := c.conn.WriteJSON(req); err != nil {
			log.Error("failed to send subscription request on heimdall ws subscription", "err", err)
			continue
		}
		log.Info("Successfully connected on heimdall ws subscription")
		return
	}
}

// readMessages continuously reads messages from the websocket, handling reconnections if necessary.
func (c *HeimdallWSClient) readMessages(ctx context.Context) {
	defer close(c.events)
	for {
		// Check if the context or unsubscribe signal is set.
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
			// continue to process messages
		}

		if err := c.conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			log.Error("failed to set read deadline on heimdall ws subscription", "err", err)

			c.tryUntilSubscribeMilestoneEvents(ctx)
			continue
		}

		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Error("connection lost; will attempt to reconnect on heimdall ws subscription", "error", err)

			c.tryUntilSubscribeMilestoneEvents(ctx)
			continue
		}

		var resp wsResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			// Skip messages that don't match the expected format.
			continue
		}

		// Find the milestone event.
		var milestoneEvent *wsEvent
		for _, event := range resp.Result.Data.Value.FinalizeBlock.Events {
			if event.Type == "milestone" {
				// In this case their types are set to the types of the respective iteration values
				// and their scope is the block of the "for" statement; they are re-used in each iteration.
				e := event
				milestoneEvent = &e
				break
			}
		}
		if milestoneEvent == nil {
			continue
		}

		// Map attributes for easier lookup.
		attrs := make(map[string]string)
		for _, attr := range milestoneEvent.Attributes {
			attrs[attr.Key] = attr.Value
		}

		// Build the Milestone object from attributes.
		m := &milestone.Milestone{
			Proposer:    common.HexToAddress(attrs["proposer"]),
			Hash:        common.HexToHash(attrs["hash"]),
			BorChainID:  attrs["bor_chain_id"],
			MilestoneID: attrs["milestone_id"],
		}
		if startBlock, err := strconv.ParseUint(attrs["start_block"], 10, 64); err == nil {
			m.StartBlock = startBlock
		}
		if endBlock, err := strconv.ParseUint(attrs["end_block"], 10, 64); err == nil {
			m.EndBlock = endBlock
		}
		if timestamp, err := strconv.ParseUint(attrs["timestamp"], 10, 64); err == nil {
			m.Timestamp = timestamp
		}

		// Deliver the milestone event, respecting context cancellation.
		select {
		case c.events <- m:
		case <-ctx.Done():
			return
		}
	}
}

// Unsubscribe signals the reader goroutine to stop.
func (c *HeimdallWSClient) Unsubscribe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		// Already unsubscribed.
	default:
		close(c.done)
	}
	return nil
}

// Close cleanly terminates the websocket connection.
func (c *HeimdallWSClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}
