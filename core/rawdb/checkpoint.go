package rawdb

import (
	json "github.com/json-iterator/go"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

var (
	lastCheckpoint = []byte("LastCheckpoint")
)

type Checkpoint struct {
	Block uint64
	Hash  common.Hash
}

func ReadLastCheckpoint(db ethdb.KeyValueReader) (uint64, common.Hash, error) {
	data, err := db.Get(lastCheckpoint)
	if len(data) == 0 {
		//todo: add context to the error
		return 0, common.Hash{}, err
	}

	lastCheckpointV := new(Checkpoint)

	if err = json.Unmarshal(data, lastCheckpointV); err != nil {
		log.Error("Unable to unmarshal the last Checkpoint block number in database", "err", err)
		return 0, common.Hash{}, err
	}

	return lastCheckpointV.Block, lastCheckpointV.Hash, nil
}

func WriteLastCheckpoint(db ethdb.KeyValueWriter, block uint64, hash common.Hash) {

	lastCheckpointV := new(Checkpoint)

	lastCheckpointV.Block = block
	lastCheckpointV.Hash = hash

	enc, err := json.Marshal(lastCheckpointV)
	if err != nil {
		log.Crit("Failed to marshal the lastCheckpoint struct", "err", err)
	}

	if err := db.Put(lastCheckpoint, enc); err != nil {
		log.Crit("Failed to store the lastCheckpoint struct", "err", err)
	}
}
