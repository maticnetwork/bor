package rawdb

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestWriteReadReservedTxIndexes_RoundTrip(t *testing.T) {
	t.Parallel()

	db := NewMemoryDatabase()
	hash := common.BytesToHash([]byte{0x01})
	number := uint64(7)

	WriteReservedTxIndexes(db, hash, number, []uint64{0, 2, 5})

	got := ReadReservedTxIndexes(db, hash, number)
	require.Equal(t, []uint64{0, 2, 5}, got)
}

func TestWriteReservedTxIndexes_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	db := NewMemoryDatabase()
	hash := common.BytesToHash([]byte{0x02})
	number := uint64(1)

	WriteReservedTxIndexes(db, hash, number, nil)

	has, err := db.Has(reservedTxIndexesKey(number, hash))
	require.NoError(t, err)
	require.False(t, has, "writing an empty index list must not grow hot storage")

	WriteReservedTxIndexes(db, hash, number, []uint64{})
	has, err = db.Has(reservedTxIndexesKey(number, hash))
	require.NoError(t, err)
	require.False(t, has)
}

func TestReadReservedTxIndexes_Absent(t *testing.T) {
	t.Parallel()

	db := NewMemoryDatabase()
	got := ReadReservedTxIndexes(db, common.BytesToHash([]byte{0x03}), 9)
	require.Nil(t, got)
}

func TestDeleteReservedTxIndexes(t *testing.T) {
	t.Parallel()

	db := NewMemoryDatabase()
	hash := common.BytesToHash([]byte{0x04})
	number := uint64(3)

	WriteReservedTxIndexes(db, hash, number, []uint64{1})
	require.NotNil(t, ReadReservedTxIndexes(db, hash, number))

	DeleteReservedTxIndexes(db, hash, number)
	require.Nil(t, ReadReservedTxIndexes(db, hash, number))

	// Deleting an already-absent entry must not error.
	DeleteReservedTxIndexes(db, hash, number)
}

// TestReadReservedTxIndexes_Malformed covers the read-side validation matrix:
// a structurally invalid entry is treated as absent (nil), never trusted into
// deriving reserved status for the wrong transaction.
func TestReadReservedTxIndexes_Malformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data []byte
	}{
		{"unsorted", mustEncodeUint64s(t, []uint64{2, 1})},
		{"duplicate", mustEncodeUint64s(t, []uint64{1, 1, 2})},
		{"oversized", make([]byte, maxReservedTxIndexesEncodedSize+1)},
		{"not RLP list of uint64", []byte{0x01, 0x02, 0x03}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			db := NewMemoryDatabase()
			hash := common.BytesToHash([]byte{0x05})
			number := uint64(11)

			require.NoError(t, db.Put(reservedTxIndexesKey(number, hash), tc.data))
			require.Nil(t, ReadReservedTxIndexes(db, hash, number))
		})
	}
}

// TestReadReservedTxIndexesBounded covers the out-of-range check that only a
// caller holding the block's transaction count can make.
func TestReadReservedTxIndexesBounded(t *testing.T) {
	t.Parallel()

	db := NewMemoryDatabase()
	hash := common.BytesToHash([]byte{0x06})
	number := uint64(4)

	WriteReservedTxIndexes(db, hash, number, []uint64{0, 3})

	// In range: txCount 4 covers indexes [0,3].
	require.Equal(t, []uint64{0, 3}, ReadReservedTxIndexesBounded(db, hash, number, 4))

	// Out of range: txCount 3 does not cover index 3; the whole entry is
	// treated as absent, not just the offending index.
	require.Nil(t, ReadReservedTxIndexesBounded(db, hash, number, 3))

	// Absent entry stays absent regardless of txCount.
	require.Nil(t, ReadReservedTxIndexesBounded(db, common.BytesToHash([]byte{0x07}), number, 100))
}

func mustEncodeUint64s(t *testing.T, v []uint64) []byte {
	t.Helper()

	data, err := rlp.EncodeToBytes(v)
	require.NoError(t, err)

	return data
}
