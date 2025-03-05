package heimdallws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/gorilla/websocket"
)

type HeimdallWSClient struct {
	conn   *websocket.Conn
	events chan *milestone.Milestone
	done   chan struct{}
	mu     sync.Mutex
}

// NewHeimdallWSClient creates a new WS client for Heimdall.
func NewHeimdallWSClient(url string) (*HeimdallWSClient, error) {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to dial websocket: %w", err)
	}
	return &HeimdallWSClient{
		conn:   conn,
		events: make(chan *milestone.Milestone),
		done:   make(chan struct{}),
	}, nil
}

// SubscribeMilestoneEvents sends the subscription request and starts processing incoming messages.
func (c *HeimdallWSClient) SubscribeMilestoneEvents(ctx context.Context) (<-chan *milestone.Milestone, error) {
	// Build the subscription request (same as your websocat command).
	req := subscriptionRequest{
		JSONRPC: "2.0",
		Method:  "subscribe",
		ID:      0,
	}
	req.Params.Query = "tm.event='NewBlock' AND milestone.number>0"

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send subscription request: %w", err)
	}

	// Start a goroutine to read and process messages.
	go c.readMessages(ctx)
	return c.events, nil
}

// readMessages continuously reads messages from the websocket, parses the milestone event,
// and pushes a Milestone object into the events channel.
func (c *HeimdallWSClient) readMessages(ctx context.Context) {
	defer close(c.events)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		default:
			c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, message, err := c.conn.ReadMessage()
			if err != nil {
				// In production, consider logging or handling reconnection.
				return
			}

			var resp wsResponse
			if err := json.Unmarshal(message, &resp); err != nil {
				// Skip messages that don't match the expected format.
				continue
			}

			// Iterate through the events in result_finalize_block to find the milestone event.
			var milestoneEvent *wsEvent
			for _, event := range resp.Result.Data.Value.FinalizeBlock.Events {
				if event.Type == "milestone" {
					milestoneEvent = &event
					break
				}
			}
			if milestoneEvent == nil {
				// No milestone event found.
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

			// Deliver the milestone event, respecting the context cancellation.
			select {
			case c.events <- m:
			case <-ctx.Done():
				return
			}
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
