package whitelist

import (
	"sync"

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

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last milestone voted on the heimdall chain
func (m *milestone) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	// We want to validate the chain by comparing the last milestoned block
	// we're storing in `milestone` with the peer's block.
	m.m.Lock()
	defer m.m.Unlock()

	return isValidPeer(remoteHeader, fetchHeadersByNumber, m.doExist, m.milestoneNumber, m.milestoneHash)

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
		return false
	}

	return isValidChain(currentHeader, chain, m.doExist, m.milestoneNumber, m.milestoneHash, m.interval)

}

func (m *milestone) ProcessMilestone(endBlockNum uint64, endBlockHash common.Hash) {
	m.m.Lock()
	defer m.m.Unlock()

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

// This function will Lock the mutex at the time of voting
func (m *milestone) LockMutex(endBlockNum uint64) bool {
	m.m.Lock()

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

// This function will unlock the mutex locked in LockMutex
func (m *milestone) UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash) {
	m.Locked = m.Locked || doLock

	if doLock {
		m.LockedSprintHash = endBlockHash
		m.LockedMilestoneIds[milestoneId] = true
	}

	m.m.Unlock()
}

// This function will unlock the locked sprint
func (m *milestone) UnlockSprint(endBlockNum uint64) {

	if endBlockNum < m.LockedSprintNumber {
		return
	}
	m.Locked = false
	m.purgeMilestoneIDsList()
}

// This function will remove the stored milestoneID
func (m *milestone) RemoveMilestoneID(milestoneId string) {
	m.m.Lock()

	delete(m.LockedMilestoneIds, milestoneId)

	if len(m.LockedMilestoneIds) == 0 {
		m.Locked = false
	}
	m.m.Unlock()
}

// This will check whether the incoming chain matches the locked sprint hash
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

// This will return the list of milestoneIDs stored.
func (m *milestone) GetMilestoneIDsList() []string {

	keys := []string{}
	for key, _ := range m.LockedMilestoneIds {
		keys = append(keys, key)
	}
	return keys

}

// This is remove the milestoneIDs stored in the list.
func (m *milestone) purgeMilestoneIDsList() {

	for k := range m.LockedMilestoneIds {
		delete(m.LockedMilestoneIds, k)
	}

}
