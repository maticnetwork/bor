package eth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/checkpoint"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
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
func (h *ethHandler) fetchWhitelistCheckpoint(ctx context.Context, bor *bor.Bor) (result *checkpoint.Checkpoint, err error) {
	defer func() {
		if err == nil && result != nil {
			log.Debug("Got new checkpoint from heimdall", "start", result.StartBlock, "end", result.EndBlock, "rootHash", result.RootHash.String())
		}
	}()

	// fetch the latest checkpoint from Heimdall
	result, err = bor.HeimdallClient.FetchCheckpoint(ctx, -1)
	if err = reportCommonErrors("latest checkpointV2", err, errCheckpoint); err != nil {
		return nil, err
	}
	return result, nil
}

func (h *ethHandler) handleWhitelistCheckpoint(ctx context.Context, checkpoint *checkpoint.Checkpoint, eth *Ethereum, verifier *borVerifier, process bool) (common.Hash, error) {
	// Verify if the checkpoint fetched can be added to the local whitelist entry or not
	// If verified, it returns the hash of the end block of the checkpoint. If not,
	// it will return appropriate error.
	hash, err := verifier.verify(ctx, eth, h, checkpoint.StartBlock, checkpoint.EndBlock, checkpoint.RootHash.String()[2:], true)
	if err != nil {
		if errors.Is(err, errChainOutOfSync) {
			log.Info("Whitelisting checkpoint deferred", "err", err)
		} else {
			log.Warn("Failed to whitelist checkpoint", "err", err)
		}
		return common.Hash{}, err
	}

	blockNum := checkpoint.EndBlock
	blockHash := common.HexToHash(hash)

	if process {
		h.downloader.ProcessCheckpoint(blockNum, blockHash)
	}

	return blockHash, nil
}

// fetchWhitelistMilestone fetches the latest milestone from it's local heimdall
// and verifies the data against bor data.
func (h *ethHandler) fetchWhitelistMilestone(ctx context.Context, bor *bor.Bor) (result *milestone.Milestone, err error) {
	defer func() {
		if err == nil && result != nil {
			log.Debug("Got new milestone from heimdall", "start", result.StartBlock, "end", result.EndBlock, "hash", result.Hash.String())
		}
	}()

	// fetch latest milestone
	result, err = bor.HeimdallClient.FetchMilestone(ctx)
	if err = reportCommonErrors("latest milestone", err, errMilestone); err != nil {
		return nil, err
	}
	return result, nil
}

// handleMilestone verify and process the fetched milestone
func (h *ethHandler) handleMilestone(ctx context.Context, eth *Ethereum, milestone *milestone.Milestone, verifier *borVerifier) error {
	// Verify if the milestone fetched can be added to the local whitelist entry or not. If verified,
	// the hash of the end block of the milestone is returned else appropriate error is returned.
	_, err := verifier.verify(ctx, eth, h, milestone.StartBlock, milestone.EndBlock, milestone.Hash.String()[2:], false)
	if err != nil {
		if errors.Is(err, errChainOutOfSync) {
			log.Info("Whitelisting milestone deferred", "err", err)
		} else {
			log.Warn("Failed to whitelist milestone", "err", err)
		}
		h.downloader.UnlockSprint(milestone.EndBlock)
	}

	num := milestone.EndBlock
	hash := milestone.Hash

	// If the current chain head is behind the received milestone, add it to the future milestone
	// list. Also, the hash mismatch (end block hash) error will lead to rewind so also
	// add that milestone to the future milestone list.
	if errors.Is(err, errChainOutOfSync) || errors.Is(err, errHashMismatch) {
		h.downloader.ProcessFutureMilestone(num, hash)
	}

	if errors.Is(err, heimdall.ErrServiceUnavailable) {
		return nil
	}

	if err != nil {
		return err
	}

	start := milestone.StartBlock

	for start <= milestone.EndBlock {
		block := eth.blockchain.GetBlockByNumber(start)
		if block != nil && block.Header() != nil {
			MilestoneWhitelistedDelayTimer.UpdateSince(time.Unix(int64(block.Time()), 0))
		}
		start += 1
	}

	h.downloader.ProcessMilestone(num, hash)

	return nil
}

// reportCommonErrors reports common errors which can occur while fetching data from heimdall. It also
// returns back the wrapped erorr if required to the caller.
func reportCommonErrors(msg string, err error, wrapError error, ctx ...interface{}) error {
	if err == nil {
		return err
	}

	// We're skipping extra check to the `heimdall.ErrServiceUnavailable` error as it should not
	// occur post HF (in heimdall). If it does, we'll anyways warn below as a normal error.

	ctx = append(ctx, "err", err)

	if strings.Contains(err.Error(), "context deadline exceeded") {
		log.Warn(fmt.Sprintf("Failed to fetch %s, please check the heimdall endpoint and status of your heimdall node", msg), ctx...)
	} else {
		log.Warn(fmt.Sprintf("Failed to fetch %s", msg), ctx...)
	}

	if wrapError != nil {
		return fmt.Errorf("%w: %v", wrapError, err)
	}

	return err
}
