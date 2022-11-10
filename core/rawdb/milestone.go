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

func ReadLastMilestone(db ethdb.KeyValueReader) (*Milestone, error) {
	data, err := db.Get(lastMilestone)
	if len(data) == 0 {
		//todo: add context to the error
		return nil, err
	}

	lastMilestoneV := new(Milestone)

	if err = json.Unmarshal(data, lastMilestoneV); err != nil {
		log.Error("Invalid pivot block number in database", "err", err)
		return nil, err
	}

	return lastMilestoneV, nil
}

func WriteLastMilestone(db ethdb.KeyValueWriter, lastMilestoneV *Milestone) {
	enc, err := json.Marshal(lastMilestoneV)
	if err != nil {
		log.Crit("Failed to encode pivot block number", "err", err)
	}

	if err := db.Put(lastMilestone, enc); err != nil {
		log.Crit("Failed to store pivot block number", "err", err)
	}
}
