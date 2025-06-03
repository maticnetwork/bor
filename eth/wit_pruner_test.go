// witpruner_test.go
package eth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/golang/mock/gomock"
)

var headerPrefix = []byte("h") // headerPrefix + num (uint64 big endian) + hash -> header

// ----------------------------------------------------------------
// Test 1: “No existing cursor” and “latest ≤ threshold” → writes cursor = 0.
// ----------------------------------------------------------------
func TestPruneWitness_NoExistingCursor_LatestLeThreshold_WritesZero(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDB := filters.NewMockDatabase(ctrl)
	mockBatch := filters.NewMockBatch(ctrl)

	mockBackend := filters.NewMockBackend(ctrl)
	mockBackend.
		EXPECT().
		HeaderByNumber(gomock.Any(), rpc.LatestBlockNumber).
		Return(&types.Header{Number: big.NewInt(5)}, nil).
		Times(1)

	api := ethapi.NewBlockChainAPI(mockBackend)

	// no cursor
	mockDB.
		EXPECT().
		Get(rawdb.WitnessPruneCursorKey).
		Return(nil, errors.New("not found")).
		Times(1)

	mockDB.
		EXPECT().
		NewBatch().
		Return(mockBatch).
		Times(1)

	zeroBuf := make([]byte, 8)
	mockBatch.
		EXPECT().
		Put(rawdb.WitnessPruneCursorKey, zeroBuf).
		Times(1)

	mockBatch.
		EXPECT().
		Write().
		Return(nil).
		Times(1)

	threshold := uint64(10)
	interval := time.Millisecond
	wp := NewWitPruner(api, mockDB, threshold, interval)

	wp.pruneWitness()
}

// ----------------------------------------------------------------
// Test 2: “Existing cursor > cutoff” → rewrites the same cursor (no deletes).
// ----------------------------------------------------------------
func TestPruneWitness_ExistingCursorGreaterThanCutoff_WritesSameValue(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDB := filters.NewMockDatabase(ctrl)
	mockBatch := filters.NewMockBatch(ctrl)

	mockBackend := filters.NewMockBackend(ctrl)
	mockBackend.
		EXPECT().
		HeaderByNumber(gomock.Any(), rpc.LatestBlockNumber).
		Return(&types.Header{Number: big.NewInt(15)}, nil).
		Times(1)

	api := ethapi.NewBlockChainAPI(mockBackend)

	existingCursor := uint64(20)
	existingBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(existingBuf, existingCursor)

	mockDB.
		EXPECT().
		Get(rawdb.WitnessPruneCursorKey).
		Return(existingBuf, nil).
		Times(1)

	mockDB.
		EXPECT().
		NewBatch().
		Return(mockBatch).
		Times(1)

	//  20 > 5, no deletes; rewrite prune-cursor = 20.
	mockBatch.
		EXPECT().
		Put(rawdb.WitnessPruneCursorKey, existingBuf).
		Times(1)

	mockBatch.
		EXPECT().
		Write().
		Return(nil).
		Times(1)

	threshold := uint64(10)
	interval := time.Millisecond
	wp := NewWitPruner(api, mockDB, threshold, interval)

	wp.pruneWitness()
}

// ----------------------------------------------------------------
// Test 3: “Existing cursor < cutoff” → deletes all hashes in [cursor, cutoff-1], then updates cursor to cutoff.
// ----------------------------------------------------------------
func TestPruneWitness_ExistingCursorLessThanCutoff_DeletesThenUpdates(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockDB := filters.NewMockDatabase(ctrl)
	mockBatch := filters.NewMockBatch(ctrl)

	mockBackend := filters.NewMockBackend(ctrl)
	mockBackend.
		EXPECT().
		HeaderByNumber(gomock.Any(), rpc.LatestBlockNumber).
		Return(&types.Header{Number: big.NewInt(100)}, nil).
		Times(1)

	api := ethapi.NewBlockChainAPI(mockBackend)

	initialCursor := uint64(80)
	initialBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(initialBuf, initialCursor)

	mockDB.
		EXPECT().
		Get(rawdb.WitnessPruneCursorKey).
		Return(initialBuf, nil).
		Times(1)

	mockIter := filters.NewMockIterator(ctrl)
	mockDB.
		EXPECT().
		NewIterator(gomock.Any(), gomock.Any()).
		Return(mockIter).
		Times(1)

	blockNumber1 := uint64(85)
	blockNumber2 := uint64(88)

	numBuf1 := make([]byte, 8)
	binary.BigEndian.PutUint64(numBuf1, blockNumber1)
	hashBytes1 := bytes.Repeat([]byte{0xAA}, 32)
	key1 := append(append(headerPrefix, numBuf1...), hashBytes1...)

	numBuf2 := make([]byte, 8)
	binary.BigEndian.PutUint64(numBuf2, blockNumber2)
	hashBytes2 := bytes.Repeat([]byte{0xBB}, 32)
	key2 := append(append(headerPrefix, numBuf2...), hashBytes2...)

	gomock.InOrder(
		// first iteration
		mockIter.
			EXPECT().
			Next().
			Return(true).
			Times(1),
		mockIter.
			EXPECT().
			Key().
			Return(key1).
			Times(1),

		// second iteration
		mockIter.
			EXPECT().
			Next().
			Return(true).
			Times(1),
		mockIter.
			EXPECT().
			Key().
			Return(key2).
			Times(1),

		// end of iteration
		mockIter.
			EXPECT().
			Next().
			Return(false).
			Times(1),

		mockIter.
			EXPECT().
			Release().
			Times(1),
	)
	mockDB.
		EXPECT().
		NewBatch().
		Return(mockBatch).
		Times(1)

	// Should DeleteWitness for hash1 and hash2:
	hash1 := common.BytesToHash(hashBytes1)
	deleteKey1 := append(rawdb.WitnessPrefix, hash1.Bytes()...)
	mockBatch.
		EXPECT().
		Delete(deleteKey1).
		Times(1)

	hash2 := common.BytesToHash(hashBytes2)
	deleteKey2 := append(rawdb.WitnessPrefix, hash2.Bytes()...)
	mockBatch.
		EXPECT().
		Delete(deleteKey2).
		Times(1)

	updatedCursor := uint64(90)
	updatedBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(updatedBuf, updatedCursor)

	mockBatch.
		EXPECT().
		Put(rawdb.WitnessPruneCursorKey, updatedBuf).
		Times(1)

	mockBatch.
		EXPECT().
		Write().
		Return(nil).
		Times(1)

	threshold := uint64(10)
	interval := time.Millisecond
	wp := NewWitPruner(api, mockDB, threshold, interval)

	wp.pruneWitness()
}
