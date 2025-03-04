package bor

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
)

// SpanStore acts as a simple middleware to cache span data populated from heimdall. It is used
// in multiple places of bor consensus for verification.
type SpanStore struct {
	store             *lru.ARCCache
	latestKnownSpanId uint64
	heimdallClient    IHeimdallClient
}

func NewSpanStore(heimdallClient IHeimdallClient) SpanStore {
	cache, _ := lru.NewARC(10)
	return SpanStore{
		store:          cache,
		heimdallClient: heimdallClient,
	}
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
		if currentSpan.Span.ID > s.latestKnownSpanId {
			s.latestKnownSpanId = currentSpan.ID
		}
	}

	return currentSpan, nil
}

// spanByBlockNumber returns a span given a block number. It fetches span from heimdall if not found in cache. It
// assumes that a span has been committed before (i.e. is current or past span) and returns an error if
// asked for a future span. This is safe to assume as we don't have a way to find out span id for a future block
// unless we hardcode the span length (which we don't want to).
func (s *SpanStore) spanByBlockNumber(ctx context.Context, blockNumber uint64) (*span.HeimdallSpan, error) {
	// Iterate over all spans and check for number. This is to replicate the behaviour implemented in
	// https://github.com/maticnetwork/genesis-contracts/blob/master/contracts/BorValidatorSet.template#L118-L134
	// This logic is independent of the span length (bit extra effort but maintains equivalence) and will work
	// for all span lengths (even if we change it in future).
	for id := s.latestKnownSpanId; id >= 0; id-- {
		span, err := s.spanById(ctx, id)
		if err != nil {
			return nil, err
		}
		if blockNumber >= span.StartBlock && blockNumber <= span.EndBlock {
			return span, nil
		}
		// Check if block number given is out of bounds
		if id == s.latestKnownSpanId && blockNumber > span.EndBlock {
			break
		}
	}

	return nil, fmt.Errorf("span not found for block %d", blockNumber)
}
