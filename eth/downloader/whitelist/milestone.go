package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/flags"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

type milestone struct {
	finality[*rawdb.Milestone]

	LockedSprintNumber uint64              // Locked sprint number
	LockedSprintHash   common.Hash         //Hash for the locked endBlock
	Locked             bool                //
	LockedMilestoneIDs map[string]struct{} //list of milestone ids

	FutureMilestoneList  map[uint64]common.FutureMilestone // Future Milestone list
	FutureMilestoneOrder []uint64                          // Future Milestone Order
	MaxCapacity          int                               //Capacity of future Milestone list
}

type milestoneService interface {
	finalityService

	GetMilestoneIDsList() []string
	RemoveMilestoneID(milestoneId string)
	LockMutex(endBlockNum uint64) bool
	UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash)
	UnlockSprint(endBlockNum uint64)
	ProcessFutureMilestone(startBlockNum uint64, endBlockNum uint64, rootHash string)
}

// IsValidChain checks the validity of chain by comparing it
// against the local milestone entries
func (m *milestone) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	m.finality.RLock()
	defer m.finality.RUnlock()

	//Checking for the milestone flag
	if !flags.Milestone {
		return true
	}

	if !m.finality.IsValidChain(currentHeader, chain) {
		return false
	}

	if m.Locked && !m.IsReorgAllowed(chain, m.LockedSprintNumber, m.LockedSprintHash) {
		return false
	}

	return true
}

// IsValidPeer checks if the chain we're about to receive from a peer is valid or not
// in terms of reorgs. We won't reorg beyond the last bor finality submitted to mainchain.
func (m *milestone) IsValidPeer(fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	if !flags.Milestone {
		return true, nil
	}

	return m.finality.IsValidPeer(fetchHeadersByNumber)
}

func (m *milestone) Process(block uint64, hash common.Hash) {
	m.finality.Lock()
	defer m.finality.Unlock()

	m.finality.Process(block, hash)

	for i := 0; i < len(m.FutureMilestoneOrder); i++ {
		if m.FutureMilestoneOrder[i] <= block {
			m.dequeueFutureMilestone()
		} else {
			break
		}
	}

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

	err := rawdb.WriteLockField(m.db, m.Locked, m.LockedSprintNumber, m.LockedSprintHash, m.LockedMilestoneIDs)
	if err != nil {
		log.Error("Error in writing lock data of milestone to db", "err", err)
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

	err := rawdb.WriteLockField(m.db, m.Locked, m.LockedSprintNumber, m.LockedSprintHash, m.LockedMilestoneIDs)

	if err != nil {
		log.Error("Error in writing lock data of milestone to db", "err", err)
	}
}

// This function will remove the stored milestoneID
func (m *milestone) RemoveMilestoneID(milestoneId string) {
	m.finality.Lock()

	delete(m.LockedMilestoneIDs, milestoneId)

	if len(m.LockedMilestoneIDs) == 0 {
		m.Locked = false
	}

	err := rawdb.WriteLockField(m.db, m.Locked, m.LockedSprintNumber, m.LockedSprintHash, m.LockedMilestoneIDs)
	if err != nil {
		log.Error("Error in writing lock data of milestone to db", "err", err)
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

func (m *milestone) IsFutureMilestoneCompatible(currentHeader *types.Header, chain []*types.Header) bool {
	chainTipNumber := chain[len(chain)-1].Number.Uint64()

	for i := 0; i < len(m.FutureMilestoneOrder); i++ {

		if chainTipNumber < m.FutureMilestoneOrder[i] {

			if i == 0 {
				return true
			}

			for j := len(chain) - 1; j >= 0; j-- {
				if chain[j].Number.Uint64() == m.FutureMilestoneOrder[i-1] {

					endBlockNum := m.FutureMilestoneOrder[i-1]
					startBlockNum := m.FutureMilestoneList[endBlockNum].Start
					milestoneRootHash := m.FutureMilestoneList[endBlockNum].Hash

					if j < int(endBlockNum-startBlockNum) {
						return true
					}

					end := j
					start := j - int(endBlockNum-startBlockNum)

					chainRootHash, err := getRootHash(chain[start : end+1])

					if err != nil {
						return true
					}

					return chainRootHash[2:] == milestoneRootHash
				}
			}
		}
	}
	return true
}

func (m *milestone) ProcessFutureMilestone(startBlockNum uint64, endBlockNum uint64, rootHash string) {

	futureMilestone := common.FutureMilestone{
		Start: startBlockNum,
		End:   endBlockNum,
		Hash:  rootHash,
	}

	if len(m.FutureMilestoneOrder) < m.MaxCapacity {
		m.enqueueFutureMilestone(endBlockNum, futureMilestone)
	}
}

// EnqueueFutureMilestone add the future milestone to the list
func (m *milestone) enqueueFutureMilestone(key uint64, futureMilestone common.FutureMilestone) {

	if _, ok := m.FutureMilestoneList[key]; !ok {
		log.Debug("Enqueing new future milestone", "end block number", key, "root hash", futureMilestone.Hash)

		m.FutureMilestoneList[key] = futureMilestone
		m.FutureMilestoneOrder = append(m.FutureMilestoneOrder, key)

		err := rawdb.WriteFutureMilestoneList(m.db, m.FutureMilestoneOrder, m.FutureMilestoneList)
		if err != nil {
			log.Error("Error in writing future milestone data to db", "err", err)
		}
	}
}

// DequeueFutureMilestone remove the future milestone entry from the list.
func (m *milestone) dequeueFutureMilestone() {
	delete(m.FutureMilestoneList, m.FutureMilestoneOrder[0])
	m.FutureMilestoneOrder = m.FutureMilestoneOrder[1:]

	err := rawdb.WriteFutureMilestoneList(m.db, m.FutureMilestoneOrder, m.FutureMilestoneList)
	if err != nil {
		log.Error("Error in writing future milestone data to db", "err", err)
	}
}
