package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type milestone struct {
	finality[*rawdb.Milestone]

	//todo: need persistence
	LockedSprintNumber uint64              // Locked sprint number
	LockedSprintHash   common.Hash         //Hash for the locked endBlock
	Locked             bool                //
	LockedMilestoneIDs map[string]struct{} //list of milestone ids

	b bool
}

// IsValidChain checks the validity of chain by comparing it
// against the local milestone entries
func (m *milestone) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	m.finality.RLock()
	defer m.finality.RUnlock()

	if !m.finality.IsValidChain(currentHeader, chain) {
		return false
	}

	if m.Locked && !m.IsReorgAllowed(chain, m.LockedSprintNumber, m.LockedSprintHash) {
		return false
	}

	return true
}

func (m *milestone) Process(block uint64, hash common.Hash) {
	m.finality.Lock()
	defer m.finality.Unlock()

	m.finality.Process(block, hash)

	m.UnlockSprint(block)
}

// This function will Lock the mutex at the time of voting
// fixme: get rid of it
func (m *milestone) LockMutex(endBlockNum uint64) bool {
	m.finality.Lock()

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

	m.finality.Unlock()
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
	m.finality.Lock()

	delete(m.LockedMilestoneIDs, milestoneId)

	if len(m.LockedMilestoneIDs) == 0 {
		m.Locked = false
	}

	m.finality.Unlock()
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
	m.finality.RLock()
	defer m.finality.RUnlock()

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
