package rawdb

import (
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// reservedTxsPrefix + num (uint64 big endian) + hash -> RLP []uint64 of
// reserved (fee-free) transaction indexes within the block, strictly
// ascending. This is derived, non-consensus data: it lets receipt reads
// report the correct effective gas price for reserved transactions without
// re-deriving the classification from the registry at read time (see
// core/types/receipt.go DeriveFields).
var reservedTxsPrefix = []byte("matic-reserved-txs-")

// maxReservedTxIndexesEncodedSize caps the encoded size accepted before
// decoding a reserved-tx index entry. A block holds at most a few thousand
// transactions, so the legitimate encoding is a few bytes per index; this cap
// is generous headroom against a corrupt or hand-edited database entry.
const maxReservedTxIndexesEncodedSize = 64 * 1024

// reservedTxIndexesKey = reservedTxsPrefix + num (uint64 big endian) + hash
func reservedTxIndexesKey(number uint64, hash common.Hash) []byte {
	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, number)

	return append(append(reservedTxsPrefix, enc...), hash.Bytes()...)
}

// ReadReservedTxIndexesRLP retrieves the RLP-encoded reserved-tx index list
// for a block: hot KV first, then ancients gated on canonical status,
// mirroring ReadBorReceiptRLP.
func ReadReservedTxIndexesRLP(db ethdb.Reader, hash common.Hash, number uint64) rlp.RawValue {
	data, _ := db.Get(reservedTxIndexesKey(number, hash))
	if len(data) != 0 {
		return data
	}

	err := db.ReadAncients(func(reader ethdb.AncientReaderOp) error {
		if isCanon(reader, number, hash) {
			data, _ = reader.Ancient(ChainFreezerReservedTxsTable, number)
		}
		return nil
	})
	if err != nil {
		log.Warn("Unable to read reserved-tx indexes rlp", "number", number, "hash", hash, "err", err)
	}

	return data
}

// decodeReservedTxIndexes decodes and structurally validates an encoded
// reserved-tx index list: capped size, and strictly ascending (which also
// rules out duplicates). It does not check indexes against the block's
// transaction count, since it has no way to know it; see
// ReadReservedTxIndexesBounded for that.
func decodeReservedTxIndexes(data []byte) ([]uint64, error) {
	if len(data) > maxReservedTxIndexesEncodedSize {
		return nil, fmt.Errorf("encoded size %d exceeds cap %d", len(data), maxReservedTxIndexesEncodedSize)
	}

	var indexes []uint64
	if err := rlp.DecodeBytes(data, &indexes); err != nil {
		return nil, err
	}

	for i := 1; i < len(indexes); i++ {
		if indexes[i] <= indexes[i-1] {
			return nil, fmt.Errorf("indexes not strictly ascending at position %d: %d <= %d", i, indexes[i], indexes[i-1])
		}
	}

	return indexes, nil
}

// ReadReservedTxIndexes retrieves the reserved (fee-free) transaction indexes
// for a block. A structurally malformed entry (oversized, unsorted, or
// duplicated) is treated as absent and logged, rather than trusted into
// deriving reserved status for the wrong transaction.
func ReadReservedTxIndexes(db ethdb.Reader, hash common.Hash, number uint64) []uint64 {
	data := ReadReservedTxIndexesRLP(db, hash, number)
	if len(data) == 0 {
		return nil
	}

	indexes, err := decodeReservedTxIndexes(data)
	if err != nil {
		log.Warn("Invalid reserved-tx index entry, treating as absent", "number", number, "hash", hash, "err", err)
		return nil
	}

	return indexes
}

// ReadReservedTxIndexesBounded is ReadReservedTxIndexes plus a check against
// txCount, the one validation ReadReservedTxIndexes cannot make on its own
// since it has no way to know the block's transaction count. Indexes are
// strictly ascending by the time this runs, so checking the last one is
// sufficient. Callers that already hold the block body (and so the count for
// free) should prefer this over the unbounded variant.
func ReadReservedTxIndexesBounded(db ethdb.Reader, hash common.Hash, number uint64, txCount int) []uint64 {
	indexes := ReadReservedTxIndexes(db, hash, number)
	if len(indexes) == 0 {
		return indexes
	}

	if indexes[len(indexes)-1] >= uint64(txCount) {
		log.Warn("Reserved-tx index entry out of range, treating as absent", "number", number, "hash", hash, "txCount", txCount, "maxIndex", indexes[len(indexes)-1])
		return nil
	}

	return indexes
}

// IsReservedTxIndex reports whether idx is among the block's reserved
// (fee-free) transaction indexes. It is the single owner of the membership
// check: indexes are only guaranteed sorted after ReadReservedTxIndexes'
// validation, so callers must not binary-search the raw slice themselves.
func IsReservedTxIndex(db ethdb.Reader, hash common.Hash, number uint64, idx uint64) bool {
	_, ok := slices.BinarySearch(ReadReservedTxIndexes(db, hash, number), idx)
	return ok
}

// WriteReservedTxIndexes stores the reserved (fee-free) transaction indexes
// for a block. Indexes must be sorted ascending and unique (guaranteed by the
// classification producing them); it is a no-op for an empty list, so hot
// storage only grows for blocks that actually have reserved transactions.
func WriteReservedTxIndexes(db ethdb.KeyValueWriter, hash common.Hash, number uint64, indexes []uint64) {
	if len(indexes) == 0 {
		return
	}

	data, err := rlp.EncodeToBytes(indexes)
	if err != nil {
		log.Crit("Failed to encode reserved-tx indexes", "err", err)
	}

	if err := db.Put(reservedTxIndexesKey(number, hash), data); err != nil {
		log.Crit("Failed to store reserved-tx indexes", "err", err)
	}
}

// DeleteReservedTxIndexes removes the reserved-tx index entry associated with
// a block hash. It is a no-op if no entry exists, matching WriteReservedTxIndexes
// only ever writing a non-empty one.
func DeleteReservedTxIndexes(db ethdb.KeyValueWriter, hash common.Hash, number uint64) {
	if err := db.Delete(reservedTxIndexesKey(number, hash)); err != nil {
		log.Crit("Failed to delete reserved-tx indexes", "err", err)
	}
}
