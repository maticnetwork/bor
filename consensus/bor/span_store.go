package bor

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
)

// SpanStore acts as a simple middleware to cache span data populated from heimdall. It is used
// in multiple places of bor consensus for verification.
type SpanStore struct {
	store *lru.ARCCache

	heimdallClient IHeimdallClient
	spanner        Spanner

	latestKnownSpanId uint64
	chainId           string
}

func NewSpanStore(heimdallClient IHeimdallClient, spanner Spanner, chainId string) SpanStore {
	cache, _ := lru.NewARC(10)
	return SpanStore{
		store:             cache,
		heimdallClient:    heimdallClient,
		spanner:           spanner,
		latestKnownSpanId: 0,
		chainId:           chainId,
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
		if s.heimdallClient == nil {
			if spanId == 0 {
				currentSpan, err = getMockSpan0(ctx, s.spanner, s.chainId)
				if err != nil {
					log.Warn("Unable to fetch span from heimdall", "id", spanId, "err", err)
					return nil, err
				}
			} else {
				return nil, fmt.Errorf("unable to create test span without heimdall client for id %d", spanId)
			}
		} else {
			currentSpan, err = s.heimdallClient.Span(ctx, spanId)
			if err != nil {
				log.Warn("Unable to fetch span from heimdall", "id", spanId, "err", err)
				return nil, err
			}
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
	isFutureSpan := false
	var id int
	for id = int(s.latestKnownSpanId); id >= 0; id-- {
		span, err := s.spanById(ctx, uint64(id))
		if err != nil {
			return nil, err
		}
		if blockNumber >= span.StartBlock && blockNumber <= span.EndBlock {
			return span, nil
		}
		// Check if block number given is out of bounds
		if id == int(s.latestKnownSpanId) && blockNumber > span.EndBlock {
			isFutureSpan = true
			break
		}
	}
	if isFutureSpan {
		log.Info("Asked for a future span, trying to fetch it from heimdall", "number", blockNumber, "lastKnownSpanId", s.latestKnownSpanId)
		id := s.latestKnownSpanId + 1
		for {
			if id > s.latestKnownSpanId+10 {
				break
			}
			log.Info("Asking for span by id", "id", id)
			span, err := s.spanById(ctx, uint64(id))
			if err != nil {
				return nil, err
			}
			if blockNumber >= span.StartBlock && blockNumber <= span.EndBlock {
				return span, nil
			}
			id++
		}
	}

	return nil, fmt.Errorf("span not found for block %d", blockNumber)
}

// setHeimdallClient sets the underlying heimdall client to be used. It is useful in
// tests where mock heimdall client is set after creation of bor instance explicitly.
func (s *SpanStore) setHeimdallClient(client IHeimdallClient) {
	s.heimdallClient = client
}

// getMockSpan0 constructs a mock span 0 by fetching validator set from genesis state. This should
// only be used in tests where heimdall client is not available.
func getMockSpan0(ctx context.Context, spanner Spanner, chainId string) (*span.HeimdallSpan, error) {
	if spanner == nil {
		return nil, fmt.Errorf("spanner not available to fetch validator set")
	}

	// Fetch validators from genesis state
	vals, err := spanner.GetCurrentValidatorsByBlockNrOrHash(ctx, rpc.BlockNumberOrHashWithNumber(0), 0)
	if err != nil {
		return nil, err
	}
	validatorSet := valset.ValidatorSet{
		Validators: vals,
		Proposer:   vals[0],
	}
	selectedProducers := make([]valset.Validator, len(vals))
	for _, v := range vals {
		selectedProducers = append(selectedProducers, *v)
	}
	return &span.HeimdallSpan{
		Span: span.Span{
			ID:         0,
			StartBlock: 0,
			EndBlock:   255,
		},
		ValidatorSet:      validatorSet,
		SelectedProducers: selectedProducers,
		ChainID:           chainId,
	}, nil
}
