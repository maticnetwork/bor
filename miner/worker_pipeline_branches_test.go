package miner

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestWorkerPipelineControlBranches(t *testing.T) {
	t.Run("speculative handler stops after setup failure", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		req.parentRoot = common.HexToHash("0xdead")
		w.handleSpeculativeWork(req)
		require.Zero(t, w.pendingWorkBlock.Load())
	})

	t.Run("missing parent state is reported", func(t *testing.T) {
		w, _, _ := newPipelineRequestFixture(t, nil)
		header := &types.Header{
			ParentHash: common.HexToHash("0xdead"),
			Number:     big.NewInt(2),
		}
		params := &generateParams{}
		statedb, err := w.resolveStateFor(header, params)
		require.EqualError(t, err, "parent block not found")
		require.Nil(t, statedb)

		env, err := w.makeEnv(header, common.Address{}, false, params)
		require.EqualError(t, err, "parent block not found")
		require.Nil(t, env)
	})

	t.Run("nil interrupt timer is a no-op", func(t *testing.T) {
		stop := createInterruptTimer(1, time.Now(), nil, nil, true)
		require.NotPanics(t, stop)
	})
}
