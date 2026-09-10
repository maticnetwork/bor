package rawdb

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/ethdb"
)

const InvalidPreconfQueryLimit = 1024

type InvalidPreconfRecord struct {
	Number uint64 `json:"number"`
	Reason string `json:"reason"`
}

func invalidPreconfKey(number uint64) []byte {
	key := make([]byte, len(invalidPreconfPrefix)+8)
	copy(key, invalidPreconfPrefix)
	binary.BigEndian.PutUint64(key[len(invalidPreconfPrefix):], ^number)
	return key
}

// PrepareInvalidPreconf adds an invalidation to batch.
func PrepareInvalidPreconf(batch ethdb.KeyValueWriter, number uint64, reason string) error {
	return batch.Put(invalidPreconfKey(number), []byte(reason))
}

// WriteInvalidPreconf atomically writes an invalidation.
func WriteInvalidPreconf(db ethdb.Database, number uint64, reason string) error {
	batch := db.NewBatch()
	if err := PrepareInvalidPreconf(batch, number, reason); err != nil {
		return err
	}
	return batch.Write()
}

func ReadInvalidPreconfs(db ethdb.Iteratee, limit uint64) []InvalidPreconfRecord {
	if limit == 0 {
		return []InvalidPreconfRecord{}
	}

	if limit > InvalidPreconfQueryLimit {
		limit = InvalidPreconfQueryLimit
	}
	iterator := db.NewIterator(invalidPreconfPrefix, nil)
	defer iterator.Release()

	records := make([]InvalidPreconfRecord, 0, limit)
	for iterator.Next() {
		key := iterator.Key()
		if len(key) != len(invalidPreconfPrefix)+8 {
			continue
		}
		records = append(records, InvalidPreconfRecord{
			Number: ^binary.BigEndian.Uint64(key[len(invalidPreconfPrefix):]),
			Reason: string(iterator.Value()),
		})
		if uint64(len(records)) == limit {
			break
		}
	}
	return records
}

// ReadInvalidPreconfsInRange returns the invalid-preconfirmation records whose
// block number falls within [from, to] inclusive, newest block first. At most
// InvalidPreconfQueryLimit records are returned so a wide range cannot produce
// an unbounded response.
func ReadInvalidPreconfsInRange(db ethdb.Iteratee, from, to uint64) []InvalidPreconfRecord {
	if from > to {
		return []InvalidPreconfRecord{}
	}

	// Keys are stored as ^number (see invalidPreconfKey), so ascending key
	// iteration yields descending block numbers. Seek to ^to — the smallest
	// key in the window — and walk upward until the decoded number drops
	// below `from`, rather than scanning the whole prefix.
	seek := make([]byte, 8)
	binary.BigEndian.PutUint64(seek, ^to)
	iterator := db.NewIterator(invalidPreconfPrefix, seek)
	defer iterator.Release()

	records := make([]InvalidPreconfRecord, 0)
	for iterator.Next() {
		key := iterator.Key()
		if len(key) != len(invalidPreconfPrefix)+8 {
			continue
		}
		number := ^binary.BigEndian.Uint64(key[len(invalidPreconfPrefix):])
		if number < from {
			break
		}
		records = append(records, InvalidPreconfRecord{
			Number: number,
			Reason: string(iterator.Value()),
		})
		if uint64(len(records)) >= InvalidPreconfQueryLimit {
			break
		}
	}
	return records
}
