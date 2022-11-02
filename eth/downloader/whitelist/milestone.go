package whitelist

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type milestone struct {
	m                  sync.Mutex
	milestoneHash      common.Hash // milestone, populated by reaching out to heimdall
	milestoneNumber    uint64      // Milestone order, populated by reaching out to heimdall
	interval           uint64      // Milestone interval, until which we can allow importing
	doExist            bool
	LockedSprintNumber uint64          // Locked sprint number
	LockedSprintHash   common.Hash     //Hash for the locked endBlock
	Locked             bool            //
	LockedMilestoneIds map[string]bool //list of milestone ids

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
	defer m.m.Unlock()

	// Check for availaibility of the last milestone block.
	// This can be also be empty if our heimdall is not responding
	// or we're running without it.
	if !m.doExist {
		// worst case, we don't have the milestone in memory
		return true, nil
	}

	// Fetch the last milestone entry
	lastMilestoneBlockNum := m.milestoneNumber
	lastMilestoneBlockHash := m.milestoneHash

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

	// Return if we've received empty chain
	if len(chain) == 0 {
		return false
	}

	m.m.Lock()
	defer m.m.Unlock()

	if m.Locked && !m.IsReorgAllowed(chain) {
		log.Warn("Sprint is locked so could not allow the Reorg")
		return false
	}

	// Check if we have milestone to validate incoming chain in memory
	if !m.doExist {
		// We don't have any entry, no additional validation will be possible
		return true
	}

	lastMilestoneBlockNum := m.milestoneNumber
	current := currentHeader.Number.Uint64()

	// Check if we have milestoneList entries in required range
	if chain[len(chain)-1].Number.Uint64() < lastMilestoneBlockNum {
		// We have future milestone entries, so we don't need to receive the past chain

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
		log.Warn("test failed", "hash", m.milestoneHash)
		return false
	}

	// Iterate over the chain and validate against the last milestone
	// It will handle all cases when the incoming chain has atleast one milestone
	for i := len(pastChain) - 1; i >= 0; i-- {
		if pastChain[i].Number.Uint64() == m.milestoneNumber {
			log.Warn("In Chain valid", "hash", m.milestoneHash)
			return pastChain[i].Hash() == m.milestoneHash
		}
	}
	log.Warn("Passed the test", "hash", m.milestoneHash)
	return true
}

func (m *milestone) ProcessMilestone(endBlockNum uint64, endBlockHash common.Hash) {
	m.m.Lock()
	defer m.m.Unlock()

	log.Warn("Processing Milestone", "endBlockNum", endBlockNum, "endBlockHash", endBlockHash)

	m.doExist = true
	m.milestoneNumber = endBlockNum
	m.milestoneHash = endBlockHash
	m.UnlockSprint(endBlockNum)

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

func (m *milestone) LockMutex(endBlockNum uint64) bool {
	m.m.Lock()

	log.Warn("In lockMutex function ")
	if m.doExist && endBlockNum <= m.milestoneNumber { //if endNum is less than whitelisted milestone, then we won't lock the sprint
		log.Warn("endBlockNum <= m.milestoneNumber")
		return false
	}

	if m.Locked && endBlockNum != m.LockedSprintNumber {
		if endBlockNum < m.LockedSprintNumber {
			log.Warn("endBlockNum < m.LockedSprintNumber")
			return false
		}

		log.Warn("endBlockNum > m.LockedSprintNumber")
		m.UnlockSprint(m.LockedSprintNumber)
		m.Locked = false
	}

	m.LockedSprintNumber = endBlockNum

	return true
}

func (m *milestone) UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash) {
	m.Locked = m.Locked || doLock
	log.Warn("In UnlockMutex function")

	if doLock {
		m.LockedSprintHash = endBlockHash
		m.LockedMilestoneIds[milestoneId] = true
	}

	m.m.Unlock()
}

func (m *milestone) UnlockSprint(endBlockNum uint64) {

	if endBlockNum < m.LockedSprintNumber {
		return
	}

	log.Warn("Unlocking the Sprint")                                                           // Need to remove this afterward
	log.Warn("Size of the milestoneList before unclocking", "size", len(m.LockedMilestoneIds)) // Need to remove this afterward

	m.Locked = false
	m.PurgeMilestoneIDsList()

	log.Warn("Size of the milestoneList after unclocking", "size", len(m.LockedMilestoneIds)) // Need to remove this afterward

}

func (m *milestone) RemoveMilestoneID(milestoneId string) {
	m.m.Lock()
	log.Warn("Removing the MilestoneID from the milestone list", "MilestoneID", milestoneId) // Just for testing

	delete(m.LockedMilestoneIds, milestoneId)

	if len(m.LockedMilestoneIds) == 0 {
		m.Locked = false
	}
	m.m.Unlock()
}

func (m *milestone) IsReorgAllowed(chain []*types.Header) bool {

	if chain[len(chain)-1].Number.Uint64() <= m.LockedSprintNumber { //Can't reorg if the end block of incoming
		return false //chain is less than locked sprint number
	}

	for i := 0; i < len(chain); i++ {
		if chain[i].Number.Uint64() == m.LockedSprintNumber {
			return chain[i].Hash() == m.LockedSprintHash

		}
	}

	return true
}

func (m *milestone) GetMilestoneIDsList() []string {

	keys := []string{}
	for key, _ := range m.LockedMilestoneIds {
		keys = append(keys, key)
	}
	return keys

}

func (m *milestone) PurgeMilestoneIDsList() {

	for k := range m.LockedMilestoneIds {
		delete(m.LockedMilestoneIds, k)
	}

}

// createMockChain returns a chain with dummy headers
// starting from `start` to `end` (inclusive)
func (m *milestone) createMockChain(start, end uint64) []*types.Header {
	var (
		i     uint64
		idx   uint64
		chain []*types.Header = make([]*types.Header, end-start+1)
	)

	for i = start; i <= end; i++ {
		header := &types.Header{
			Number: big.NewInt(int64(i)),
			Time:   uint64(time.Now().UnixMicro()) + i,
		}
		chain[idx] = header
		idx++
	}

	return chain
}
