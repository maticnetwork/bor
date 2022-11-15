package whitelist

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type DummyService struct{}

func (DummyService) IsValidPeer(remoteHeader *types.Header, fetchHeadersByNumber func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error)) (bool, error) {
	return true, nil
}

func (DummyService) IsValidChain(currentHeader *types.Header, chain []*types.Header) bool {
	return true
}

func (DummyService) ProcessCheckpoint(endBlockNum uint64, endBlockHash common.Hash) {}
func (DummyService) ProcessMilestone(endBlockNum uint64, endBlockHash common.Hash)  {}

func (DummyService) PurgeWhitelistedCheckpoint() {}
func (DummyService) PurgeWhitelistedMilestone()  {}

// todo: check if we use it somewhere alse then PrivateAPI
func (DummyService) GetWhitelistedCheckpoint() (bool, uint64, common.Hash) {
	return true, 0, common.Hash{}
}

func (DummyService) GetWhitelistedMilestone() (bool, uint64, common.Hash) {
	return true, 0, common.Hash{}
}

func (DummyService) Purge() {
}

func (DummyService) LockMutex(endBlockNum uint64) bool {
	return false
}

func (DummyService) UnlockMutex(doLock bool, milestoneId string, endBlockHash common.Hash) {
}

func (DummyService) UnlockSprint(endBlockNum uint64) {
}

func (DummyService) RemoveMilestoneID(milestoneId string) {
}

func (DummyService) GetMilestoneIDsList() []string {
	return []string{}
}
