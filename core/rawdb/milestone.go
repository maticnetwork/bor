package rawdb

import (
	"fmt"

	json "github.com/json-iterator/go"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/gererics"
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

func (m *Milestone) clone() *Milestone {
	return &Milestone{}
}

func (m *Milestone) block() (uint64, common.Hash) {
	return m.Block, m.Hash
}

func ReadFinality[T BlockFinality[T]](db ethdb.KeyValueReader) (uint64, common.Hash, error) {
	lastTV, key := getKey[T]()

	data, err := db.Get(key)
	if err != nil {
		return 0, common.Hash{}, fmt.Errorf("%w: empty response for %s", err, string(key))
	}

	if len(data) == 0 {
		return 0, common.Hash{}, fmt.Errorf("%w for %s", ErrEmptyLastFinality, string(key))
	}

	if err = json.Unmarshal(data, lastTV); err != nil {
		log.Error(fmt.Sprintf("Unable to unmarshal the last %s block number in database", string(key)), "err", err)

		return 0, common.Hash{}, fmt.Errorf("%w(%v) for %s, data %v(%q)",
			ErrIncorrectFinality, err, string(key), data, string(data))
	}

	block, hash := lastTV.block()

	return block, hash, nil
}

func WriteLastFinality[T BlockFinality[T]](db ethdb.KeyValueWriter, block uint64, hash common.Hash) error {
	lastTV, key := getKey[T]()

	lastTV.set(block, hash)

	enc, err := json.Marshal(lastTV)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to marshal the %s struct", string(key)), "err", err)

		return fmt.Errorf("%w: %v for %s struct", ErrIncorrectFinalityToStore, err, string(key))
	}

	if err := db.Put(key, enc); err != nil {
		log.Error(fmt.Sprintf("Failed to store the %s struct", string(key)), "err", err)

		return fmt.Errorf("%w: %v for %s struct", ErrDBNotResponding, err, string(key))
	}

	return nil
}

type BlockFinality[T any] interface {
	set(block uint64, hash common.Hash)
	clone() T
	block() (uint64, common.Hash)
}

func getKey[T BlockFinality[T]]() (T, []byte) {
	lastT := gererics.Empty[T]().clone()

	var key []byte

	switch any(lastT).(type) {
	case *Milestone:
		key = lastMilestone
	case *Checkpoint:
		key = lastCheckpoint
	}

	return lastT, key
}
