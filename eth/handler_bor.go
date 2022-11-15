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

	// errNoAckMilestone is returned when we are unable to fetch the
	// latest no-ack milestone from the local heimdall.
	errNoAckMilestone = errors.New("failed to fetch no ack milestone")

	//errHardFork is returned when chain hasn't reached to the specified
	//hard forked number.
	errHardFork = errors.New("Chain hasn't reached to the hard forked number")
)

// fetchWhitelistCheckpoint fetches the latest checkpoint from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistCheckpoint(ctx context.Context, bor *bor.Bor, eth *Ethereum, verifier *borVerifier) (uint64, common.Hash, error) {
	var (
		blockNum  uint64
		blockHash common.Hash
	)

	chainConfig := eth.blockchain.GetChainConfig()
	currentBlock := eth.blockchain.CurrentBlock().NumberU64()

	if !chainConfig.Bor.IsDubai(currentBlock) {
		return blockNum, blockHash, errHardFork
	}

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

	chainConfig := eth.blockchain.GetChainConfig()
	currentBlock := eth.blockchain.CurrentBlock().NumberU64()
	log.Error("In the fetchWhitelisting A")
	if !chainConfig.Bor.IsDubai(currentBlock) {
		log.Error("Failed", "Hard Fork Number", chainConfig.Bor.DubaiBlock)
		return blockNum, blockHash, errHardFork
	}
	log.Error("In the fetchWhitelisting B")

	// fetch latest milestone
	milestone, err := bor.HeimdallClient.FetchMilestone(ctx)
	if err != nil {
		log.Error("Failed to fetch latest milestone for whitelisting", "err", err)
		return blockNum, blockHash, errMilestone
	}

	// Verify if the milestone fetched can be added to the local whitelist entry or not
	// If verified, it returns the hash of the end block of the milestone. If not,
	// it will return appropriate error.
	hash, err := verifier.verify(ctx, eth, h, milestone.StartBlock.Uint64(), milestone.EndBlock.Uint64(), milestone.RootHash.String()[2:])
	if err != nil {
		h.downloader.UnlockSprint(milestone.EndBlock.Uint64())
		return blockNum, blockHash, err
	}

	blockNum = milestone.EndBlock.Uint64()
	blockHash = common.HexToHash(hash)

	return blockNum, blockHash, nil
}

func (h *ethHandler) fetchNoAckMilestone(ctx context.Context, bor *bor.Bor) (string, error) {
	var (
		milestoneID string
	)

	// fetch latest milestone
	milestoneID, err := bor.HeimdallClient.FetchLastNoAckMilestone(ctx)
	if err != nil {
		log.Error("Failed to fetch latest no-ack milestone", "err", err)

		return milestoneID, errMilestone
	}

	return milestoneID, nil
}

func (h *ethHandler) fetchNoAckMilestoneByID(ctx context.Context, bor *bor.Bor, milestoneID string) error {
	// fetch latest milestone
	err := bor.HeimdallClient.FetchNoAckMilestone(ctx, milestoneID)

	// fixme: handle different types of errors
	if errors.Is(err, ErrNotInRejectedList) {
		// todo:
		log.Warn("Failed to fetch no-ack milestone", "milestoneID", milestoneID, "err", err)
	}
	if err != nil {
		log.Error("Failed to fetch no-ack milestone", "milestoneID", milestoneID, "err", err)

		return errMilestone
	}

	return nil
}
