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
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to dial websocket: %w", err)
	}
	return &HeimdallWSClient{
		conn:   conn,
		url:    url,
		events: make(chan *milestone.Milestone),
		done:   make(chan struct{}),
	}, nil
}

// SubscribeMilestoneEvents sends the subscription request and starts processing incoming messages.
func (c *HeimdallWSClient) SubscribeMilestoneEvents(ctx context.Context) (<-chan *milestone.Milestone, error) {
	// Build the subscription request.
	req := subscriptionRequest{
		JSONRPC: "2.0",
		Method:  "subscribe",
		ID:      0,
	}
	req.Params.Query = "tm.event='NewBlock' AND milestone.number>0"

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send subscription request: %w", err)
	}

	// Start the goroutine to read messages.
	go c.readMessages(ctx)
	return c.events, nil
}

// readMessages continuously reads messages from the websocket, handling reconnections if necessary.
func (c *HeimdallWSClient) readMessages(ctx context.Context) {
	defer close(c.events)
	for {
		// Check if the context or unsubscribe signal is set.
		select {
		case <-ctx.Done():
			log.Info("#################### Context Done")
			return
		case <-c.done:
			log.Info("#################### Client Done")
			return
		default:
			// continue to process messages
		}

		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Error("Connection lost; will attempt to reconnect on Heimdall WS subscription", "error", err)
			log.Info("#################### Error in WS connection!", "receivedErr", err)

			c.Reconnect(ctx)
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
				milestoneEvent = &event
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
			log.Info("############## New attribute!!!", "attribKey", attr.Key, "attribValue", attr.Value)
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

func (c *HeimdallWSClient) Reconnect(ctx context.Context) {
	for {
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

		time.Sleep(10 * time.Second)

		// Attempt to reconnect.
		newConn, _, err := websocket.DefaultDialer.Dial(c.url, nil)
		if err != nil {
			log.Error("Reconnection attempt failed on Heimdall WS subscription", "error", err)
			log.Info("#################### Failed to recover connection!", "receivedErr", err)
			continue
		}

		// Replace the connection.
		c.mu.Lock()
		c.conn = newConn
		c.mu.Unlock()
		log.Info("Successfully reconnected on Heimdall WS subscription")
		log.Info("#################### Successfully recovered connection!", "receivedErr", err)

		// Re-send the subscription request.
		req := subscriptionRequest{
			JSONRPC: "2.0",
			Method:  "subscribe",
			ID:      0,
		}
		req.Params.Query = "tm.event='NewBlock' AND milestone.number>0"
		if err := c.conn.WriteJSON(req); err != nil {
			log.Error("Failed to re-send subscription request on Heimdall WS subscription", "error", err)
			continue
		}
		break
	}
}
