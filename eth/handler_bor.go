package eth

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/log"
)

var (
	// errCheckpoint is returned when we are unable to fetch the
	// latest checkpoint from the local heimdall.
	errCheckpoint = errors.New("failed to fetch latest checkpoint")

	// errMilestone is returned when we are unable to fetch the
	// latest milestone from the local heimdall.
	errMilestone = errors.New("failed to fetch latest milestone")
)

// fetchWhitelistCheckpoint fetches the latest checkpoint from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistCheckpoint(ctx context.Context, bor *bor.Bor, eth *Ethereum, verifier *borVerifier) (uint64, common.Hash, error) {
	var (
		blockNum  uint64
		blockHash common.Hash
	)

	// fetch the latest checkpoint from Heimdall
	checkpoint, err := bor.HeimdallClient.FetchCheckpoint(ctx, -1)
	if err != nil {
		log.Debug("Failed to fetch latest checkpoint for whitelisting", "err", err)
		return blockNum, blockHash, errCheckpoint
	}

	// Verify if the checkpoint fetched can be added to the local whitelist entry or not
	// If verified, it returns the hash of the end block of the checkpoint. If not,
	// it will return appropriate error.
	hash, err := verifier.verify(ctx, eth, h, checkpoint.StartBlock.Uint64(), checkpoint.EndBlock.Uint64(), checkpoint.RootHash.String()[2:])
	if err != nil {
		log.Warn("Failed to whitelist checkpoint", "err", err)
		return blockNum, blockHash, err
	}

	blockNum = checkpoint.EndBlock.Uint64()
	blockHash = common.HexToHash(hash)

	return blockNum, blockHash, nil
}

// fetchWhitelistMilestone fetches the latest milestone from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistMilestone(ctx context.Context, bor *bor.Bor, eth *Ethereum, verifier *borVerifier) (uint64, common.Hash, error) {
	var (
		blockNum  uint64
		blockHash common.Hash
	)

	// fetch latest milestone
	milestone, err := bor.HeimdallClient.FetchMilestone(ctx)
	if err != nil {
		log.Error("Failed to fetch latest milestone for whitelisting", "err", err)
		return blockNum, blockHash, errMilestone
	}

	log.Warn("fetching the milestone")
	// Verify if the milestone fetched can be added to the local whitelist entry or not
	// If verified, it returns the hash of the end block of the milestone. If not,
	// it will return appropriate error.
	hash, err := verifier.verify(ctx, eth, h, milestone.StartBlock.Uint64(), milestone.EndBlock.Uint64(), milestone.RootHash.String()[2:])
	if err != nil {
		h.downloader.UnlockSprint()
		log.Warn("Unlocking the Sprint when roothash mismatches")
		log.Warn("Failed to whitelist milestone", "err", err)
		return blockNum, blockHash, err
	}

	h.downloader.UnlockSprint()
	log.Warn("milestoneupdated", "Hash", blockHash)
	log.Warn("Unlocking the Sprint when roothash matches")
	blockNum = milestone.EndBlock.Uint64()
	blockHash = common.HexToHash(hash)

	return blockNum, blockHash, nil
}
