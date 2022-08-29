package whitelist

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type milestone struct {
	m               sync.Mutex
	milestoneHash   common.Hash // milestone, populated by reaching out to heimdall
	milestoneNumber uint64      // Milestone order, populated by reaching out to heimdall
	interval        uint64      // Milestone interval, until which we can allow importing
	doExist         bool
}

var (
	ErrMilestoneMismatch = errors.New("milestone mismatch")
	ErrNoRemoteMilestone = errors.New("remote peer doesn't have a milestone")
)

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last milestone voted on the heimdall chain
func (m *milestone) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	// We want to validate the chain by comparing the last milestoned block
	// we're storing in `milestone` with the peer's block.
	m.m.Lock()

	// Check for availaibility of the last milestone block.
	// This can be also be empty if our heimdall is not responding
	// or we're running without it.
	if !m.doExist {
		// worst case, we don't have the milestone in memory
		m.m.Unlock()
		return true, nil
	}

	// Fetch the last milestone entry
	lastMilestoneBlockNum := m.milestoneNumber
	lastMilestoneBlockHash := m.milestoneHash

	m.m.Unlock()

	// todo: we can extract this as an interface and mock as well or just test IsValidChain in isolation from downloader passing fake fetchHeadersByNumber functions
	headers, hashes, err := fetchHeadersByNumber(lastMilestoneBlockNum, 1, 0, false)
	if err != nil {
		return false, fmt.Errorf("%w: last milestone %d, err %v", ErrNoRemoteMilestone, lastMilestoneBlockNum, err)
	}

	if len(headers) == 0 {
		return false, fmt.Errorf("%w: last milestone %d", ErrNoRemoteMilestone, lastMilestoneBlockNum)
	}

	reqBlockNum := headers[0].Number.Uint64()
	reqBlockHash := hashes[0]

	// Check against the checkpointed blocks
	if reqBlockNum == lastMilestoneBlockNum && reqBlockHash == lastMilestoneBlockHash {
		return true, nil
	}

	return false, ErrMilestoneMismatch
}

// IsValidChain checks the validity of chain by comparing it
// against the local milestone entries
func (m *milestone) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	// Check if we have milestone to validate incoming chain in memory
	if !m.doExist {
		// We don't have any entry, no additional validation will be possible
		return true
	}

	// Return if we've received empty chain
	if len(chain) == 0 {
		return false
	}

	m.m.Lock()
	defer m.m.Unlock()

	lastMilestoneBlockNum := m.milestoneNumber
	current := currentHeader.Number.Uint64()

	// Check if we have milestoneList entries in required range
	if chain[len(chain)-1].Number.Uint64() < lastMilestoneBlockNum {
		// We have future milestone entries, so no additional validation will be possible
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
	if len(futureChain)+offset > int(m.interval) {
		return false
	}

	// Iterate over the chain and validate against the last milestone
	// It will handle all cases where the incoming chain has atleast one milestone
	for i := len(pastChain) - 1; i >= 0; i-- {
		if pastChain[i].Number.Uint64() == m.milestoneNumber {
			return pastChain[i].Hash() == m.milestoneHash
		}
	}

	return true
}

func (m *milestone) ProcessMilestone(endBlockNum uint64, endBlockHash common.Hash) {
	m.m.Lock()
	defer m.m.Unlock()

	m.doExist = true
	m.milestoneNumber = endBlockNum
	m.milestoneHash = endBlockHash
}

// GetMilestone returns the existing whitelisted
// entry of milestone
func (m *milestone) GetWhitelistedMilestone() (bool, uint64, common.Hash) {
	m.m.Lock()
	defer m.m.Unlock()

	return m.doExist, m.milestoneNumber, m.milestoneHash
}

// PurgeMilestone purges data from milestone
func (m *milestone) PurgeWhitelistedMilestone() {
	m.m.Lock()
	defer m.m.Unlock()

	m.doExist = false
}
