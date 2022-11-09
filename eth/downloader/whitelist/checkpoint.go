package whitelist

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type checkpoint struct {
	m                sync.Mutex
	checkpointHash   common.Hash // Whitelisted Checkpoint Hash, populated by reaching out to heimdall
	checkpointNumber uint64      // Checkpoint Number , populated by reaching out to heimdall
	interval         uint64      // Interval, until which we can allow importing
	doExist          bool
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor checkpoint submitted to mainchain.
func (w *checkpoint) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	// We want to validate the chain by comparing the last checkpointed block
	// we're storing in `checkpointWhitelist` with the peer's block.
	w.m.Lock()
	defer w.m.Unlock()

	return isValidPeer(remoteHeader, fetchHeadersByNumber, w.doExist, w.checkpointNumber, w.checkpointHash)

}

// IsValidChain checks the validity of chain by comparing it
// against the local checkpoint entry
func (w *checkpoint) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {

	// Return if we've received empty chain
	if len(chain) == 0 {
		return false
	}

	w.m.Lock()
	defer w.m.Unlock()

	return isValidChain(currentHeader, chain, w.doExist, w.checkpointNumber, w.checkpointHash, w.interval)
}

func (w *checkpoint) ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash) {
	w.m.Lock()
	defer w.m.Unlock()

	w.doExist = true
	w.checkpointHash = endBlockHash
	w.checkpointNumber = endBlockNum
}

// GetWhitelistedMilestone returns the existing whitelisted
// entries of checkpoint of the form (doExist,block number,block hash.)
func (w *checkpoint) GetWhitelistedCheckpoint() (bool, uint64, common.Hash) {
	w.m.Lock()
	defer w.m.Unlock()

	return w.doExist, w.checkpointNumber, w.checkpointHash
}

// PurgeWhitelistedCheckpoint purges the whitlisted checkpoint
func (w *checkpoint) PurgeWhitelistedCheckpoint() {
	w.m.Lock()
	defer w.m.Unlock()

	w.doExist = false
}
