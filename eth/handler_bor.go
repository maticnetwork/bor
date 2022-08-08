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

	// errMilestoneCount is returned when we are unable to fetch
	// the milestone count from local heimdall.
	errMilestoneCount = errors.New("failed to fetch milestone count")

	// errNoMilestone is returned when there is not milestone proposed
	// by heimdall yet or heimdall is not in sync
	errNoMilestone = errors.New("no milestone proposed")

	// errMilestone is returned when we are unable to fetch the
	// latest checkpoint from the local heimdall.
	errMilestone = errors.New("failed to fetch the milestone")

	// errMissingMilestone is returned when we don't have the
	// checkpoint blocks locally, yet.
	errMissingMilestone = errors.New("missing checkpoint blocks")

	// errMilestoneRootHashMismatch is returned when the local root hash
	// doesn't match with the root hash in milestone.
	errMilestoneRootHashMismatch = errors.New("milestone roothash mismatch")
)

// fetchWhitelistCheckpoints fetches the latest checkpoint/s from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistCheckpoint(ctx context.Context, bor *bor.Bor, first bool) (uint64, common.Hash, error) {
	// Create an array for block number and block hashes
	//nolint:prealloc
	var (
		blockNum  uint64
		blockHash common.Hash
	)

	// Fetch the checkpoint count from heimdall
	count, err := bor.HeimdallClient.FetchCheckpointCount(ctx)
	if err != nil {
		log.Debug("Failed to fetch checkpoint count for whitelisting", "err", err)
		return blockNum, blockHash, errCheckpointCount
	}

	if count == 0 {
		return blockNum, blockHash, errNoCheckpoint
	}

	// fetch the latest checkpoint from Heimdall
	checkpoint, err := bor.HeimdallClient.FetchCheckpoint(ctx)
	if err != nil {
		log.Debug("Failed to fetch latest checkpoint for whitelisting", "err", err)
		return blockNum, blockHash, errMilestone
	}

	// check if we have the checkpoint blocks
	head := h.ethAPI.BlockNumber()
	if head < hexutil.Uint64(checkpoint.EndBlock.Uint64()) {
		log.Debug("Head block behind checkpoint block", "head", head, "milestone end block", checkpoint.EndBlock)
		return blockNum, blockHash, errMissingMilestone
	}

	// verify the root hash of checkpoint
	roothash, err := h.ethAPI.GetRootHash(ctx, checkpoint.StartBlock.Uint64(), checkpoint.EndBlock.Uint64())
	if err != nil {
		log.Debug("Failed to get root hash of checkpoint while whitelisting", "err", err)
		return blockNum, blockHash, errRootHash
	}

	if roothash != checkpoint.RootHash.String()[2:] {
		log.Warn("Checkpoint root hash mismatch while whitelisting", "expected", checkpoint.RootHash.String()[2:], "got", roothash)
		return blockNum, blockHash, errCheckpointRootHashMismatch
	}

	// fetch the end checkpoint block hash
	block, err := h.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(checkpoint.EndBlock.Uint64()), false)
	if err != nil {
		log.Debug("Failed to get end block hash of checkpoint while whitelisting", "err", err)
		return blockNum, blockHash, errEndBlock
	}

	hash := fmt.Sprintf("%v", block["hash"])

	blockNum = checkpoint.EndBlock.Uint64()
	blockHash = common.HexToHash(hash)

	return blockNum, blockHash, nil
}

// fetchWhitelistMilestones fetches the latest milestone/s from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistMilestone(ctx context.Context, bor *bor.Bor, first bool) (uint64, common.Hash, error) {
	// Create an array for block number and block hashes
	//nolint:prealloc
	var (
		blockNum  uint64
		blockHash common.Hash
	)

	// Fetch the milestone count from heimdall
	count, err := bor.HeimdallClient.FetchMilestoneCount(ctx)
	if err != nil {
		log.Debug("Failed to fetch count for milestones", "err", err)
		return blockNum, blockHash, errMilestoneCount
	}

	if count == 0 {
		return blockNum, blockHash, errNoMilestone
	}

	//fetch latest milestone
	milestone, err := bor.HeimdallClient.FetchMilestone(ctx)
	if err != nil {
		log.Debug("Failed to fetch latest milestone for whitelisting", "err", err)
		return blockNum, blockHash, errMilestone
	}

	// check if we have the milestone blocks
	head := h.ethAPI.BlockNumber()
	if head < hexutil.Uint64(milestone.EndBlock.Uint64()) {
		log.Debug("Head block behind milestone block", "head", head, "milestone end block", milestone.EndBlock)
		return blockNum, blockHash, errMissingMilestone
	}

	// verify the root hash of milestone
	roothash, err := h.ethAPI.GetRootHash(ctx, milestone.StartBlock.Uint64(), milestone.EndBlock.Uint64())
	if err != nil {
		log.Debug("Failed to get root hash of milestone while whitelisting", "err", err)
		return blockNum, blockHash, errRootHash
	}

	if roothash != milestone.RootHash.String()[2:] {
		log.Warn("Milestone root hash mismatch while whitelisting", "expected", milestone.RootHash.String()[2:], "got", roothash)
		return blockNum, blockHash, errCheckpointRootHashMismatch
	}

	// fetch the end milestone block hash
	block, err := h.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(milestone.EndBlock.Uint64()), false)
	if err != nil {
		log.Debug("Failed to get end block hash of milestone while whitelisting", "err", err)
		return blockNum, blockHash, errEndBlock
	}

	hash := fmt.Sprintf("%v", block["hash"])

	blockNum = milestone.EndBlock.Uint64()
	blockHash = common.HexToHash(hash)

	return blockNum, blockHash, nil
}
