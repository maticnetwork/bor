package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/metrics"
)

type checkpoint struct {
	finality[*rawdb.Checkpoint]
}

type checkpointService interface {
	finalityService
}

var (
	//Metrics for collecting the whitelisted milestone number
	whitelistedCheckpointNumberMeter = metrics.NewRegisteredMeter("chain/checkpoint/latest", nil)
)

// IsValidChain checks the validity of chain by comparing it
// against the local checkpoint entry
func (w *checkpoint) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	w.finality.RLock()
	defer w.finality.RUnlock()

	return w.finality.IsValidChain(currentHeader, chain)
}

func (w *checkpoint) Process(block uint64, hash common.Hash) {
	w.finality.Lock()
	defer w.finality.Unlock()

	if w.finality.Number == block {
		return
	}

	w.finality.Process(block, hash)

	whitelistedCheckpointNumberMeter.Mark(int64(block))
}
