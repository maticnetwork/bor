package params

type BorParamsResponse struct {
	Height string    `json:"height"`
	Result BorParams `json:"result"`
}

type BorParams struct {
	SprintLength  uint64 `json:"sprint_duration"`
	SpanLength    uint64 `json:"span_duration"`
	ProducerCount uint64 `json:"producer_count"`
}
