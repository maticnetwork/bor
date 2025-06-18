package heimdallws

import "encoding/json"

// subscriptionRequest represents the JSON-RPC request for subscribing.
type subscriptionRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      int    `json:"id"`
	Params  struct {
		Query string `json:"query"`
	} `json:"params"`
}

// --- Structures to parse the WS response ---

// attribute represents a key/value pair in the event attributes.
type attribute struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Index bool   `json:"index"`
}

// wsEvent represents a single event in the WS response.
type wsEvent struct {
	Type       string      `json:"type"`
	Attributes []attribute `json:"attributes"`
}

// finalizeBlock corresponds to the result_finalize_block field.
type finalizeBlock struct {
	Events []wsEvent `json:"events"`
	// Other fields are omitted.
}

// wsValue represents the "value" portion of the data.
type wsValue struct {
	Block         json.RawMessage `json:"block"` // Omitted
	BlockID       json.RawMessage `json:"block_id"`
	FinalizeBlock finalizeBlock   `json:"result_finalize_block"`
}

// wsData holds the type and value returned.
type wsData struct {
	Type  string  `json:"type"`
	Value wsValue `json:"value"`
}

// wsResult holds the result object.
type wsResult struct {
	Query string `json:"query"`
	Data  wsData `json:"data"`
}

// wsResponse is the top-level response structure from the WS subscription.
type wsResponse struct {
	JSONRPC string   `json:"jsonrpc"`
	ID      int      `json:"id"`
	Result  wsResult `json:"result"`
	// "events" field is present but not needed here.
}
