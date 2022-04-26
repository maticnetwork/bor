package bor

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
	"github.com/xsleonard/go-merkle"
	"golang.org/x/crypto/sha3"
)

var (
	// MaxCheckpointLength is the maximum number of blocks that can be requested for constructing a checkpoint root hash
	MaxCheckpointLength = uint64(math.Pow(2, 15))
)

// API is a user facing RPC API to allow controlling the signer and voting
// mechanisms of the proof-of-authority scheme.
type API struct {
	chain         consensus.ChainHeaderReader
	bor           *Bor
	rootHashCache *lru.ARCCache
}

// GetSnapshot retrieves the state snapshot at a given block.
func (api *API) GetSnapshot(number *rpc.BlockNumber) (*Snapshot, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return its snapshot
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.bor.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
}

// GetAuthor retrieves the author a block.
func (api *API) GetAuthor(number *rpc.BlockNumber) (*common.Address, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return its snapshot
	if header == nil {
		return nil, errUnknownBlock
	}
	author, err := api.bor.Author(header)
	return &author, err
}

type Block struct {
	Transactions []*struct {
		Hash string `json:"hash"`
	} `json:"transactions"`
	Header       *Header      `json:"header"`
	Uncles       []*Header    `json:"uncles"`
	Hash         common.Hash  `json:"hash"`
	Size         atomic.Value `json:"size"`
	Td           uint         `json:"totalDifficulty"`
	ReceivedAt   time.Time    `json:"receivedAt"`
	ReceivedFrom interface{}  `json:"receivedFrom"`
}

type Header struct {
	Number *big.Int `json:"number"           gencodec:"required"`
}

// WriteBorTransaction add a bor transaction to rawDB
func (api *API) WriteBorTransaction(allLogsCount uint, _stateSyncLogs string, _blockData string) (bool, error) {
	var block Block

	var stateSyncLogs []*types.Log
	fmt.Println("LOLOLOL", allLogsCount)
	fmt.Println("LOLOLOL", _stateSyncLogs)
	fmt.Println("_blockdata", _blockData)

	// _blockdata refers to data of the block without state-sync transactions. It is derived from the local RPC of this same node.
	if err := json.Unmarshal([]byte(_blockData), &block); err != nil {
		fmt.Println(1)
		return false, err
	}

	if block.Header.Number.Uint64() >= api.chain.CurrentHeader().Number.Uint64() {
		fmt.Println(2)
		return false, errors.New("block number is greater than current header number")
	}

	if err := json.Unmarshal([]byte(_stateSyncLogs), &stateSyncLogs); err != nil {
		fmt.Println(3)
		return false, err
	}

	if rawdb.ReadBorReceipt(api.bor.db, block.Hash, block.Header.Number.Uint64()) != nil {
		fmt.Println(4)
		return false, errors.New("bor receipts already exists")
	}
	fmt.Println("statesynclogs", stateSyncLogs[0])

	blockBatch := api.bor.db.NewBatch()
	if allLogsCount > 0 {
		fmt.Println(len(stateSyncLogs))
		if len(stateSyncLogs) > 0 {
			// stateSyncLogs = blockLogs[len(logs):] // get state-sync logs from `state.Logs()`

			// State sync logs don't have tx index, tx hash and other necessary fields
			// DeriveFieldsForBorLogs will fill those fields for websocket subscriptions
			types.DeriveFieldsForBorLogs(stateSyncLogs, block.Hash, block.Header.Number.Uint64(), uint(len(block.Transactions)), uint(int(allLogsCount)-len(stateSyncLogs)))

			// Write bor receipt
			rawdb.WriteBorReceipt(blockBatch, block.Hash, block.Header.Number.Uint64(), &types.ReceiptForStorage{
				Status: types.ReceiptStatusSuccessful, // make receipt status successful
				Logs:   stateSyncLogs,
			})

			// Write bor tx reverse lookup
			rawdb.WriteBorTxLookupEntry(blockBatch, block.Hash, block.Header.Number.Uint64())
		}
	}
	return true, nil
}

// GetSnapshotAtHash retrieves the state snapshot at a given block.
func (api *API) GetSnapshotAtHash(hash common.Hash) (*Snapshot, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.bor.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
}

// GetSigners retrieves the list of authorized signers at the specified block.
func (api *API) GetSigners(number *rpc.BlockNumber) ([]common.Address, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return the signers from its snapshot
	if header == nil {
		return nil, errUnknownBlock
	}
	snap, err := api.bor.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
	if err != nil {
		return nil, err
	}
	return snap.signers(), nil
}

// GetSignersAtHash retrieves the list of authorized signers at the specified block.
func (api *API) GetSignersAtHash(hash common.Hash) ([]common.Address, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	snap, err := api.bor.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
	if err != nil {
		return nil, err
	}
	return snap.signers(), nil
}

// GetCurrentProposer gets the current proposer
func (api *API) GetCurrentProposer() (common.Address, error) {
	snap, err := api.GetSnapshot(nil)
	if err != nil {
		return common.Address{}, err
	}
	return snap.ValidatorSet.GetProposer().Address, nil
}

// GetCurrentValidators gets the current validators
func (api *API) GetCurrentValidators() ([]*Validator, error) {
	snap, err := api.GetSnapshot(nil)
	if err != nil {
		return make([]*Validator, 0), err
	}
	return snap.ValidatorSet.Validators, nil
}

// GetRootHash returns the merkle root of the start to end block headers
func (api *API) GetRootHash(start uint64, end uint64) (string, error) {
	if err := api.initializeRootHashCache(); err != nil {
		return "", err
	}
	key := getRootHashKey(start, end)
	if root, known := api.rootHashCache.Get(key); known {
		return root.(string), nil
	}
	length := uint64(end - start + 1)
	if length > MaxCheckpointLength {
		return "", &MaxCheckpointLengthExceededError{start, end}
	}
	currentHeaderNumber := api.chain.CurrentHeader().Number.Uint64()
	if start > end || end > currentHeaderNumber {
		return "", &InvalidStartEndBlockError{start, end, currentHeaderNumber}
	}
	blockHeaders := make([]*types.Header, end-start+1)
	wg := new(sync.WaitGroup)
	concurrent := make(chan bool, 20)
	for i := start; i <= end; i++ {
		wg.Add(1)
		concurrent <- true
		go func(number uint64) {
			blockHeaders[number-start] = api.chain.GetHeaderByNumber(uint64(number))
			<-concurrent
			wg.Done()
		}(i)
	}
	wg.Wait()
	close(concurrent)

	headers := make([][32]byte, nextPowerOfTwo(length))
	for i := 0; i < len(blockHeaders); i++ {
		blockHeader := blockHeaders[i]
		header := crypto.Keccak256(appendBytes32(
			blockHeader.Number.Bytes(),
			new(big.Int).SetUint64(blockHeader.Time).Bytes(),
			blockHeader.TxHash.Bytes(),
			blockHeader.ReceiptHash.Bytes(),
		))

		var arr [32]byte
		copy(arr[:], header)
		headers[i] = arr
	}

	tree := merkle.NewTreeWithOpts(merkle.TreeOptions{EnableHashSorting: false, DisableHashLeaves: true})
	if err := tree.Generate(convert(headers), sha3.NewLegacyKeccak256()); err != nil {
		return "", err
	}
	root := hex.EncodeToString(tree.Root().Hash)
	api.rootHashCache.Add(key, root)
	return root, nil
}

func (api *API) initializeRootHashCache() error {
	var err error
	if api.rootHashCache == nil {
		api.rootHashCache, err = lru.NewARC(10)
	}
	return err
}

func getRootHashKey(start uint64, end uint64) string {
	return strconv.FormatUint(start, 10) + "-" + strconv.FormatUint(end, 10)
}
