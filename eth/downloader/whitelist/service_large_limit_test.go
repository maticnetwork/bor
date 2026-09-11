package whitelist

import (
	"math"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/stretchr/testify/require"
)

func TestForkValidationInitialCapacity(t *testing.T) {
	require.Equal(t, 2*DefaultMaxForkCorrectnessLimit, forkValidationInitialCapacity(math.MaxUint64))
	require.Equal(t, uint64(128), forkValidationInitialCapacity(128))
}

func TestCheckForkCorrectnessLargeLimit(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	service := NewService(db, false, math.MaxUint64)

	parent := createMockChain(1, 1, common.Hash{})[0]
	rawdb.WriteHeader(db, parent)
	chain := createMockChain(2, 2, parent.Hash())
	service.ProcessCheckpoint(parent.Number.Uint64(), parent.Hash())

	// The configured traversal limit remains unchanged. Only the initial
	// blocksChecked allocation is bounded.
	require.Equal(t, uint64(math.MaxUint64), service.maxForkCorrectnessLimit)

	var valid bool
	require.NotPanics(t, func() {
		valid = service.checkForkCorrectness(chain)
	})
	require.True(t, valid)
}
