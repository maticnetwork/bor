package bor

import (
	"context"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
)

// SpanStore acts as a simple middleware to cache span data populated from heimdall. It is used
// in multiple places of bor consensus for verification.
type SpanStore struct {
	store          *lru.ARCCache
	heimdallClient IHeimdallClient
}

func NewSpanStore(heimdallClient IHeimdallClient) SpanStore {
	cache, _ := lru.NewARC(10)
	return SpanStore{
		store:          cache,
		heimdallClient: heimdallClient,
	}
}

const (
	defaultSpanLength = 6400 // Default span length i.e. number of bor blocks in a span
	zerothSpanEnd     = 255  // End block of 0th span
)

// SpanIdAt returns the corresponding span id for the given block number.
func spanIdAt(blockNum uint64) uint64 {
	if blockNum > zerothSpanEnd {
		return 1 + (blockNum-zerothSpanEnd-1)/defaultSpanLength
	}
	return 0
}

// spanById returns a span given its id. It fetches span from heimdall if not found in cache.
func (s *SpanStore) spanById(ctx context.Context, spanId uint64) (*span.HeimdallSpan, error) {
	var currentSpan *span.HeimdallSpan
	if value, ok := s.store.Get(spanId); ok {
		currentSpan, _ = value.(*span.HeimdallSpan)
	}

	if currentSpan == nil {
		var err error
		currentSpan, err = s.heimdallClient.Span(ctx, spanId)
		if err != nil {
			log.Warn("Unable to fetch span from heimdall", "id", spanId, "err", err)
			return nil, err
		}
		s.store.Add(spanId, currentSpan)
	}

	return currentSpan, nil
}

// spanByBlockNumber returns a span given a block number. It fetches span from heimdall if not found in cache.
func (s *SpanStore) spanByBlockNumber(ctx context.Context, blockNumber uint64) (*span.HeimdallSpan, error) {
	spanId := spanIdAt(blockNumber)
	return s.spanById(ctx, spanId)
}
