package whitelist

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

type milestone struct {
	db       ethdb.Database
	m        sync.RWMutex
	Hash     common.Hash // milestone, populated by reaching out to heimdall
	Number   uint64      // Milestone order, populated by reaching out to heimdall
	interval uint64      // Milestone interval, until which we can allow importing
	doExist  bool

	//todo: need persistence
	LockedSprintNumber uint64              // Locked sprint number
	LockedSprintHash   common.Hash         //Hash for the locked endBlock
	Locked             bool                //
	LockedMilestoneIDs map[string]struct{} //list of milestone ids
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last milestone voted on the Heimdall chain
func (m *milestone) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	// We want to validate the chain by comparing the last milestone block
	// we're storing in `milestone` with the peer's block.
	m.m.RLock()

	doExist := m.doExist
	milestoneNumber := m.Number
	milestoneHash := m.Hash

	m.m.RUnlock()

	return isValidPeer(remoteHeader, fetchHeadersByNumber, doExist, milestoneNumber, milestoneHash)

}

// IsValidChain checks the validity of chain by comparing it
// against the local milestone entries
func (m *milestone) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {

	// Return if we've received empty chain
	if len(chain) == 0 {
		return false
	}

	m.m.RLock()

	locked := m.Locked
	doExist := m.doExist
	milestoneNumber := m.Number
	milestoneHash := m.Hash
	interval := m.interval

	lockedSprintNumber := m.LockedSprintNumber
	lockedSprintHash := m.LockedSprintHash

	m.m.RUnlock()

	if locked && !m.IsReorgAllowed(chain, lockedSprintNumber, lockedSprintHash) {
		return false
	}

	return isValidChain(currentHeader, chain, doExist, milestoneNumber, milestoneHash, interval)
}

func (m *milestone) ProcessMilestone(block uint64, hash common.Hash) {
	m.m.Lock()
	defer m.m.Unlock()

	m.doExist = true
	m.Number = block
	m.Hash = hash
	rawdb.WriteLastMilestone(m.db, block, hash)

	m.UnlockSprint(block)
}

// GetMilestone returns the existing whitelisted
// entry of milestone
func (m *milestone) GetWhitelistedMilestone() (bool, uint64, common.Hash) {
	m.m.RLock()
	defer m.m.RUnlock()

	if m.doExist {
		return m.doExist, m.Number, m.Hash
	}

	if block, hash, err := rawdb.ReadLastMilestone(m.db); err == nil {
		return true, block, hash
	}

	return false, m.Number, m.Hash
}

// PurgeMilestone purges data from milestone
func (m *milestone) PurgeWhitelistedMilestone() {
	m.m.Lock()
	defer m.m.Unlock()

	m.doExist = false
}

// This function will Lock the mutex at the time of voting
// fixme: get rid of it
func (m *milestone) LockMutex(endBlockNum uint64) bool {
	m.m.Lock()

	if m.doExist && endBlockNum <= m.Number { //if endNum is less than whitelisted milestone, then we won't lock the sprint
		// todo: add endBlockNum and m.Number as values - the same below
		log.Warn("endBlockNum <= m.Number")
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
// fixme: get rid of it
func (m *milestone) UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash) {
	m.Locked = m.Locked || doLock

	if doLock {
		m.LockedSprintHash = endBlockHash
		m.LockedMilestoneIDs[milestoneId] = struct{}{}
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

	delete(m.LockedMilestoneIDs, milestoneId)

	if len(m.LockedMilestoneIDs) == 0 {
		m.Locked = false
	}

	m.m.Unlock()
}

// This will check whether the incoming chain matches the locked sprint hash
func (m *milestone) IsReorgAllowed(chain []*types.Header, lockedSprintNumber uint64, lockedSprintHash common.Hash) bool {

	if chain[len(chain)-1].Number.Uint64() <= lockedSprintNumber { //Can't reorg if the end block of incoming
		return false //chain is less than locked sprint number
	}

	for i := 0; i < len(chain); i++ {
		if chain[i].Number.Uint64() == lockedSprintNumber {
			return chain[i].Hash() == lockedSprintHash

		}
	}

	return true
}

// This will return the list of milestoneIDs stored.
func (m *milestone) GetMilestoneIDsList() []string {
	m.m.RLock()
	defer m.m.RUnlock()

	// fixme: use generics :)
	keys := make([]string, 0, len(m.LockedMilestoneIDs))
	for key := range m.LockedMilestoneIDs {
		keys = append(keys, key)
	}

	return keys
}

// This is remove the milestoneIDs stored in the list.
func (m *milestone) purgeMilestoneIDsList() {
	m.LockedMilestoneIDs = make(map[string]struct{})
}
