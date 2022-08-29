package whitelist

import (
	"errors"
	"fmt"
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

var (
	ErrCheckpointMismatch = errors.New("checkpoint mismatch")
	ErrNoRemoteCheckoint  = errors.New("remote peer doesn't have a checkoint")
)

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor checkpoint submitted to mainchain.
func (w *checkpoint) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	// We want to validate the chain by comparing the last checkpointed block
	// we're storing in `checkpointWhitelist` with the peer's block.
	w.m.Lock()

	// Check for availaibility of the last checkpointed block.
	// doExist will be false if our heimdall is not responding
	// or we're running without it.
	if !w.doExist {
		w.m.Unlock()
		// worst case, we don't have the checkpoint in memory
		return true, nil
	}

	// Fetch the last checkpoint entry
	lastCheckpointBlockNum := w.checkpointNumber
	lastCheckpointBlockHash := w.checkpointHash

	w.m.Unlock()

	// todo: we can extract this as an interface and mock as well or just test IsValidChain in isolation from downloader passing fake fetchHeadersByNumber functions
	headers, hashes, err := fetchHeadersByNumber(lastCheckpointBlockNum, 1, 0, false)
	if err != nil {
		return false, fmt.Errorf("%w: last checkpoint %d, err %v", ErrNoRemoteCheckoint, lastCheckpointBlockNum, err)
	}

	if len(headers) == 0 {
		return false, fmt.Errorf("%w: last checkpoint %d", ErrNoRemoteCheckoint, lastCheckpointBlockNum)
	}

	reqBlockNum := headers[0].Number.Uint64()
	reqBlockHash := hashes[0]

	// Check against the latest checkpointed block
	if reqBlockNum == lastCheckpointBlockNum && reqBlockHash == lastCheckpointBlockHash {
		return true, nil
	}

	return false, ErrCheckpointMismatch
}

// IsValidChain checks the validity of chain by comparing it
// against the local checkpoint entry
func (w *checkpoint) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	// Check if we have checkpoint to validate incoming chain in memory
	if !w.doExist {
		// We don't have whitelisted checkpoint, no additional validation will be possible
		return true
	}

	// Return if we've received empty chain
	if len(chain) == 0 {
		return false
	}

	w.m.Lock()
	defer w.m.Unlock()

	var (
		LatestCheckpointNumber uint64 = w.checkpointNumber
		current                uint64 = currentHeader.Number.Uint64()
	)

	// Check if we have whitelist entry in required range
	if chain[len(chain)-1].Number.Uint64() < LatestCheckpointNumber {
		// We have future whitelisted entry, so don't need that incoming chain
		return false
	}

	// Split the chain into past and future chain
	pastChain, futureChain := splitChain(current, chain)

	// Add an offset to future chain if it's not in continuity
	offset := 0
	if len(futureChain) != 0 {
		offset += int(futureChain[0].Number.Uint64()-currentHeader.Number.Uint64()) - 1
	}

	// Don't accept future chain of unacceptable length (from current block)
	if len(futureChain)+offset > int(w.interval) {
		return false
	}

	// Iterate over the chain and validate against the last milestone
	// It will handle all cases where the incoming chain has atleast one milestone
	for i := len(pastChain) - 1; i >= 0; i-- {
		if pastChain[i].Number.Uint64() == w.checkpointNumber {
			return pastChain[i].Hash() == w.checkpointHash
		}
	}

	return true
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
