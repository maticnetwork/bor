package rawdb

import (
	"fmt"

	json "github.com/json-iterator/go"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

var (
	lastMilestone = []byte("LastMilestone")
)

type Finality struct {
	Block uint64
	Hash  common.Hash
}

func (f *Finality) set(block uint64, hash common.Hash) {
	f.Block = block
	f.Hash = hash
}

type Milestone struct {
	Finality
}

func ReadFinality[T BlockFinality](db ethdb.KeyValueReader) (T, error) {
	lastTV, key := getKey[T]()

	data, err := db.Get(key)
	if err != nil {
		return nil, fmt.Errorf("%w: empty response for %s", err, string(key))
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("%w for %s", ErrEmptyLastFinality, string(key))
	}

	if err = json.Unmarshal(data, lastTV); err != nil {
		log.Error("Unable to unmarshal the last milestone block number in database", "err", err)

		return nil, err

		log.Error(fmt.Sprintf("Unable to unmarshal the last %s block number in database", string(key)), "err", err)

		return nil, fmt.Errorf("%w(%v) for %s, data %v(%q)",
			ErrIncorrectFinality, err, string(key), data, string(data))
	}

	return lastTV, nil
}

func WriteLastFinality[T BlockFinality](db ethdb.KeyValueWriter, block uint64, hash common.Hash) error {
	lastTV, key := getKey[T]()

	lastTV.set(block, hash)

	enc, err := json.Marshal(key)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to marshal the %s struct", string(key)), "err", err)

		return fmt.Errorf("%w: %v for %s struct", ErrIncorrectFinalityToStore, err, string(key))
	}

	if err := db.Put(key, enc); err != nil {
		log.Crit(fmt.Sprintf("Failed to store the %s struct", string(key)), "err", err)

		return fmt.Errorf("%w: %v for %s struct", ErrDBNotResponding, err, string(key))
	}

	return nil
}

type BlockFinality interface {
	set(block uint64, hash common.Hash)
}

func getKey[T BlockFinality]() (T, []byte) {
	lastT := *new(T)

	var key []byte

	switch any(lastT).(type) {
	case *Milestone:
		key = lastMilestone
	case *Checkpoint:
		key = lastCheckpoint
	}

	return lastT, key
}
