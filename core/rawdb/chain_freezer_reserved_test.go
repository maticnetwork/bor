package rawdb

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

// writeMinimalBlock writes just enough hot data (header, canonical hash,
// body, receipts, td, and optionally reserved-tx indexes) for freezeRange to
// accept the block, and returns its hash.
func writeMinimalBlock(t *testing.T, db ethdb.KeyValueStore, number uint64, parentHash common.Hash, reservedIdx []uint64) common.Hash {
	t.Helper()

	header := &types.Header{
		ParentHash: parentHash,
		Number:     new(big.Int).SetUint64(number),
		GasLimit:   8_000_000,
		Extra:      []byte{byte(number)},
	}
	hash := header.Hash()

	WriteHeader(db, header)
	WriteCanonicalHash(db, hash, number)
	WriteBody(db, hash, number, &types.Body{})
	WriteReceipts(db, hash, number, types.Receipts{})
	WriteTd(db, hash, number, new(big.Int).SetUint64(number+1))
	if len(reservedIdx) > 0 {
		WriteReservedTxIndexes(db, hash, number, reservedIdx)
	}

	return hash
}

// canonReader combines a key-value store with a chain freezer's real ancient
// data, unlike nofreezedb (whose Ancient always errors - it exists only to
// let freezeRange read the *hot* side while writing to the freezer). isCanon
// needs to see genuine ancient hash-table entries, so reads that must cross
// the hot/ancient boundary use this instead.
type canonReader struct {
	ethdb.KeyValueStore
	*chainFreezer
}

// TestChainFreezerReservedTxs_FreezeAndReadViaAncients exercises the real
// chainFreezer.freezeRange path: it must append the hot reserved-tx entry
// (empty or not) for every frozen block, exactly as it already does for the
// bor receipt table, and the block reading it back afterwards must see the
// same value via ancients (hot KV entry is gone at that point in production,
// which this test reproduces by deleting it before reading back).
func TestChainFreezerReservedTxs_FreezeAndReadViaAncients(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	kv := memorydb.New()
	nfdb := &nofreezedb{KeyValueStore: kv}

	cf, err := newChainFreezer(dir, "", "", false, 0, kv)
	require.NoError(t, err)
	defer cf.Close()

	genesisHash := writeMinimalBlock(t, kv, 0, common.Hash{}, nil)
	hash1 := writeMinimalBlock(t, kv, 1, genesisHash, []uint64{0, 2})
	hash2 := writeMinimalBlock(t, kv, 2, hash1, nil)

	hashes, err := cf.freezeRange(nfdb, 0, 2)
	require.NoError(t, err)
	require.Len(t, hashes, 3)

	// Delete the hot entries the way the real freeze() background loop does
	// post-freeze, so the reads below can only be satisfied from ancients.
	DeleteReservedTxIndexes(kv, hash1, 1)
	DeleteReservedTxIndexes(kv, hash2, 2)

	reader := &canonReader{KeyValueStore: kv, chainFreezer: cf}
	require.Equal(t, []uint64{0, 2}, ReadReservedTxIndexes(reader, hash1, 1))
	require.Nil(t, ReadReservedTxIndexes(reader, hash2, 2))
	require.Nil(t, ReadReservedTxIndexes(reader, genesisHash, 0))
}
