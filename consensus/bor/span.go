package bor

// Span represents a current bor span
type Span struct {
	ID         uint64 `json:"span_id,string" yaml:"span_id,string"`
	StartBlock uint64 `json:"start_block,string" yaml:"start_block,string"`
	EndBlock   uint64 `json:"end_block,string" yaml:"end_block,string"`
}

// HeimdallSpan represents span from heimdall APIs
type HeimdallSpan struct {
	Span
	ValidatorSet      ValidatorSet `json:"validator_set" yaml:"validator_set"`
	SelectedProducers []Validator  `json:"selected_producers" yaml:"selected_producers"`
	ChainID           string       `json:"chain_id" yaml:"chain_id"`
}

// HeimdallSpan rest response
type HeimdallSpanResponse struct {
	Span HeimdallSpan `json:"span"`
}
