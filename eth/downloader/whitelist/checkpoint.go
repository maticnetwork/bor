package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

type checkpoint struct {
	finality[*rawdb.Checkpoint]
}

type checkpointService interface {
	finalityService
}

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

	w.finality.Process(block, hash)
}

func (w *checkpoint) block() (uint64, common.Hash) {
	return w.Number, w.Hash
}
