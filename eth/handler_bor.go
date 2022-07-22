package eth

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	// errCheckpointCount is returned when we are unable to fetch
	// the checkpoint count from local heimdall.
	errCheckpointCount = errors.New("failed to fetch checkpoint count")

	// errNoCheckpoint is returned when there is not checkpoint proposed
	// by heimdall yet or heimdall is not in sync
	errNoCheckpoint = errors.New("no checkpoint proposed")

	// errCheckpoint is returned when we are unable to fetch the
	// latest checkpoint from the local heimdall.
	errCheckpoint = errors.New("failed to fetch latest checkpoint")

	// errMissingCheckpoint is returned when we don't have the
	// checkpoint blocks locally, yet.
	errMissingCheckpoint = errors.New("missing checkpoint blocks")

	// errRootHash is returned when we aren't able to calculate the root hash
	// locally for a range of blocks.
	errRootHash = errors.New("failed to get local root hash")

	// errCheckpointRootHashMismatch is returned when the local root hash
	// doesn't match with the root hash in checkpoint.
	errCheckpointRootHashMismatch = errors.New("checkpoint roothash mismatch")

	// errEndBlock is returned when we're unable to fetch a block locally.
	errEndBlock = errors.New("failed to get end block")
)

// fetchWhitelistCheckpoints fetches the latest checkpoint/s from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistCheckpoints(ctx context.Context, bor *bor.Bor, first bool) ([]uint64, []common.Hash, error) {
	// Create an array for block number and block hashes
	//nolint:prealloc
	var (
		blockNums   []uint64      = make([]uint64, 0)
		blockHashes []common.Hash = make([]common.Hash, 0)
	)

	// Fetch the checkpoint count from heimdall
	count, err := bor.HeimdallClient.FetchCheckpointCount(ctx)
	if err != nil {
		log.Debug("Failed to fetch checkpoint count for whitelisting", "err", err)
		return blockNums, blockHashes, errCheckpointCount
	}

	if count == 0 {
		return blockNums, blockHashes, errNoCheckpoint
	}

	var (
		start int64
		end   int64
	)

	// Prepare the checkpoint range to fetch
	if count <= 10 {
		start = 1
	} else {
		start = count - 10 + 1 // 10 is the max number of checkpoints to fetch
	}

	end = count

	// If we're in not in the first iteration, only fetch the latest checkpoint
	if !first {
		start = count
	}

	for i := start; i <= end; i++ {
		// fetch `i` indexed checkpoint from heimdall
		checkpoint, err := bor.HeimdallClient.FetchCheckpoint(ctx, i)
		if err != nil {
			log.Debug("Failed to fetch latest checkpoint for whitelisting", "err", err)
			return blockNums, blockHashes, errCheckpoint
		}

		// check if we have the checkpoint blocks
		head := h.ethAPI.BlockNumber()
		if head < hexutil.Uint64(checkpoint.EndBlock.Uint64()) {
			log.Debug("Head block behind checkpoint block", "head", head, "checkpoint end block", checkpoint.EndBlock)
			return blockNums, blockHashes, errMissingCheckpoint
		}

		// verify the root hash of checkpoint
		roothash, err := h.ethAPI.GetRootHash(ctx, checkpoint.StartBlock.Uint64(), checkpoint.EndBlock.Uint64())
		if err != nil {
			log.Debug("Failed to get root hash of checkpoint while whitelisting", "err", err)
			return blockNums, blockHashes, errRootHash
		}

		if roothash != checkpoint.RootHash.String()[2:] {
			log.Warn("Checkpoint root hash mismatch while whitelisting", "expected", checkpoint.RootHash.String()[2:], "got", roothash)
			return blockNums, blockHashes, errCheckpointRootHashMismatch
		}

		// fetch the end checkpoint block hash
		block, err := h.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(checkpoint.EndBlock.Uint64()), false)
		if err != nil {
			log.Debug("Failed to get end block hash of checkpoint while whitelisting", "err", err)
			return blockNums, blockHashes, errEndBlock
		}

		hash := fmt.Sprintf("%v", block["hash"])

		blockNums = append(blockNums, checkpoint.EndBlock.Uint64())
		blockHashes = append(blockHashes, common.HexToHash(hash))
	}

	return blockNums, blockHashes, nil
}
