package rawdb

import (
	json "github.com/json-iterator/go"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

var (
	lastMilestone = []byte("LastMilestone")
)

type Milestone struct {
	Block uint64
	Hash  common.Hash
}

func ReadLastMilestone(db ethdb.KeyValueReader) (uint64, common.Hash, error) {
	data, err := db.Get(lastMilestone)
	if len(data) == 0 {
		//todo: add context to the error
		return 0, common.Hash{}, err
	}

	lastMilestoneV := new(Milestone)

	if err = json.Unmarshal(data, lastMilestoneV); err != nil {
		log.Error("Unable to unmarshal the last milestone block number in database", "err", err)
		return 0, common.Hash{}, err
	}

	return lastMilestoneV.Block, lastMilestoneV.Hash, nil
}

func WriteLastMilestone(db ethdb.KeyValueWriter, block uint64, hash common.Hash) {

	lastMilestoneV := new(Milestone)

	lastMilestoneV.Block = block
	lastMilestoneV.Hash = hash

	enc, err := json.Marshal(lastMilestoneV)
	if err != nil {
		log.Crit("Failed to marshal the lastMilestone struct", "err", err)
	}

	if err := db.Put(lastMilestone, enc); err != nil {
		log.Crit("Failed to store the lastMilestone struct", "err", err)
	}
}
