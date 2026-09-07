package rawdb

import (
	"encoding/binary"
	"fmt"

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

// ReadPreconfAuditedThrough returns the highest block the sequence-store audit
// has compared against the canonical chain, and whether a watermark is stored
// at all. A node that has never audited has no watermark, which is not the same
// as having audited through block zero — and neither is the same as a database
// that could not answer, which is why a read failure is an error rather than a
// third spelling of absence.
func ReadPreconfAuditedThrough(db ethdb.KeyValueReader) (uint64, bool, error) {
	return readPreconfHeight(db, preconfAuditedThroughKey)
}

// WritePreconfAuditedThrough stores the audit watermark.
func WritePreconfAuditedThrough(db ethdb.KeyValueWriter, number uint64) error {
	return writePreconfHeight(db, preconfAuditedThroughKey, number)
}

// ReadPreconfUnauditedThrough returns the highest block the audit is known to
// have skipped. Heights at or below it may hold preconfirmations this node
// never compared against the chain, so an empty invalidation range there means
// unknown rather than clean.
func ReadPreconfUnauditedThrough(db ethdb.KeyValueReader) (uint64, bool, error) {
	return readPreconfHeight(db, preconfUnauditedThroughKey)
}

// WritePreconfUnauditedThrough raises the skipped-window mark. It never lowers
// it: a later pass auditing a narrower window does not make an older gap go
// away. An unreadable current mark is treated as absent and the write goes
// ahead — recording a gap this node knows about beats leaving the window
// unrecorded because the comparison could not be made.
func WritePreconfUnauditedThrough(db ethdb.KeyValueStore, number uint64) error {
	current, stored, err := ReadPreconfUnauditedThrough(db)
	if err == nil && stored && current >= number {
		return nil
	}
	return writePreconfHeight(db, preconfUnauditedThroughKey, number)
}

// readPreconfHeight separates the three answers a stored height can have:
// present, absent, and unavailable. Collapsing the last into the second would
// let a read failure read as "never audited", which seeds the audit watermark
// at the current head and marks an uncompared window clean.
func readPreconfHeight(db ethdb.KeyValueReader, key []byte) (uint64, bool, error) {
	present, err := db.Has(key)
	if err != nil {
		return 0, false, fmt.Errorf("read preconf height %s: %w", key, err)
	}
	if !present {
		return 0, false, nil
	}

	value, err := db.Get(key)
	if err != nil {
		return 0, false, fmt.Errorf("read preconf height %s: %w", key, err)
	}
	if len(value) != 8 {
		return 0, false, fmt.Errorf("preconf height %s is %d bytes, want 8", key, len(value))
	}

	return binary.BigEndian.Uint64(value), true, nil
}

func writePreconfHeight(db ethdb.KeyValueWriter, key []byte, number uint64) error {
	value := make([]byte, 8)
	binary.BigEndian.PutUint64(value, number)
	return db.Put(key, value)
}
