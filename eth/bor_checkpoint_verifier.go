// nolint
package eth

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	// errMissingCurrentBlock is returned when we don't have the current block
	// present locally.
	errMissingCurrentBlock = errors.New("current block missing")

	// errChainOutOfSync is returned when we're trying to process a future
	// checkpoint/milestone and we haven't reached at that number yet.
	errChainOutOfSync = errors.New("chain out of sync")

	// errRootHash is returned when the root hash calculation for a range of blocks fails.
	errRootHash = errors.New("root hash calculation failed")

	// errHashMismatch is returned when the local hash doesn't match
	// with the hash of checkpoint/milestone. It is the root hash of blocks
	// in case of checkpoint and is end block hash in case of milestones.
	errHashMismatch = errors.New("hash mismatch")

	// errEndBlock is returned when we're unable to fetch a block locally.
	errEndBlock = errors.New("failed to get end block")

	// errEndBlock is returned when we're unable to fetch the tip confirmation block locally.
	errTipConfirmationBlock = errors.New("failed to get tip confirmation block")

	// rewindLengthMeter for collecting info about the length of chain rewinded
	rewindLengthMeter = metrics.NewRegisteredMeter("chain/autorewind/length", nil)
)

const maxRewindLen uint64 = 126

type borVerifier struct {
	verify func(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, hash string, isCheckpoint bool) (string, error)
}

func newBorVerifier() *borVerifier {
	return &borVerifier{borVerify}
}

func borVerify(ctx context.Context, eth *Ethereum, handler *ethHandler, start uint64, end uint64, hash string, isCheckpoint bool) (string, error) {
	str := "milestone"
	if isCheckpoint {
		str = "checkpoint"
	}

	// check if we have the given blocks
	currentBlock := eth.BlockChain().CurrentBlock()
	if currentBlock == nil {
		log.Debug(fmt.Sprintf("Failed to fetch current block from blockchain while verifying incoming %s", str))
		return hash, errMissingCurrentBlock
	}

	head := currentBlock.Number.Uint64()

	if head < end {
		log.Debug(fmt.Sprintf("Current head block behind incoming %s block", str), "head", head, "end block", end)
		return hash, errChainOutOfSync
	}

	var localHash string

	// verify the hash
	if isCheckpoint {
		var err error

		// in case of checkpoint get the rootHash
		localHash, err = handler.ethAPI.GetRootHash(ctx, start, end)

		if err != nil {
			log.Debug("Failed to calculate root hash of given block range while whitelisting checkpoint", "start", start, "end", end, "err", err)
			return hash, fmt.Errorf("%w: %v", errRootHash, err)
		}
	} else {
		// in case of milestone(isCheckpoint==false) get the hash of endBlock
		block, err := handler.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(end), false)
		if err != nil {
			log.Debug("Failed to get end block hash while whitelisting milestone", "number", end, "err", err)
			return hash, fmt.Errorf("%w: %v", errEndBlock, err)
		}

		localHash = fmt.Sprintf("%v", block["hash"])[2:]
	}

	//nolint
	if localHash != hash {
		if isCheckpoint {
			log.Warn("Root hash mismatch while whitelisting checkpoint", "expected", localHash, "got", hash)
		} else {
			log.Warn("End block hash mismatch while whitelisting milestone", "expected", localHash, "got", hash)
		}

		ethHandler := (*ethHandler)(eth.handler)

		var (
			rewindTo uint64
			doExist  bool
		)

		// First try the original logic to find existing whitelisted milestone/checkpoint
		if doExist, rewindTo, _ = ethHandler.downloader.GetWhitelistedMilestone(); doExist {
			// Use existing whitelisted milestone
			log.Info("Using existing whitelisted milestone for rewind", "block", rewindTo)
		} else if doExist, rewindTo, _ = ethHandler.downloader.GetWhitelistedCheckpoint(); doExist {
			// Use existing whitelisted checkpoint
			log.Info("Using existing whitelisted checkpoint for rewind", "block", rewindTo)
		} else {
			// No existing whitelisted milestone/checkpoint found
			// For milestones, try to find the common ancestor by checking past milestones
			if !isCheckpoint && end > 0 {
				log.Info("No existing whitelisted milestone/checkpoint, searching for common ancestor")
				rewindTo = findCommonAncestorWithFutureMilestones(eth, start, end, hash)
			} else {
				// For checkpoints or when start is 0, use simple fallback
				if start <= 0 {
					rewindTo = 0
				} else {
					rewindTo = start - 1
				}
			}
		}

		if head-rewindTo > maxRewindLen {
			rewindTo = head - maxRewindLen
		}

		if isCheckpoint {
			log.Info("Rewinding chain due to checkpoint root hash mismatch", "number", rewindTo)
		} else {
			log.Info("Rewinding chain due to milestone endblock hash mismatch", "number", rewindTo)
		}

		var canonicalChain []*types.Block

		length := end - rewindTo

		canonicalChain = eth.BlockChain().GetBlocksFromHash(common.HexToHash(hash), int(length))

		// Reverse the canonical chain
		for i, j := 0, len(canonicalChain)-1; i < j; i, j = i+1, j-1 {
			canonicalChain[i], canonicalChain[j] = canonicalChain[j], canonicalChain[i]
		}

		rewindBack(eth, head, rewindTo)

		if canonicalChain != nil && len(canonicalChain) == int(length) {
			log.Info("Inserting canonical chain", "from", canonicalChain[0].NumberU64(), "hash", canonicalChain[0].Hash(), "to", canonicalChain[len(canonicalChain)-1].NumberU64(), "hash", canonicalChain[len(canonicalChain)-1].Hash())
			_, err := eth.BlockChain().InsertChain(canonicalChain)
			if err != nil {
				log.Warn("Failed to insert canonical chain", "err", err)
				return hash, err
			}

			// Then explicitly set the final block as canonical to ensure proper canonical mapping
			finalBlock := canonicalChain[len(canonicalChain)-1]
			_, err = eth.BlockChain().SetCanonical(finalBlock)
			if err != nil {
				log.Warn("Failed to set canonical head after insertion", "err", err, "block", finalBlock.NumberU64(), "hash", finalBlock.Hash())
				return hash, err
			}

			log.Info("Successfully inserted canonical chain")
		} else {
			log.Warn("Failed to insert canonical chain", "length", len(canonicalChain), "expected", length)
			// If is okay if we can't find the canonical chain
			return hash, nil
		}
	}

	// fetch the end block hash
	block, err := handler.ethAPI.GetBlockByNumber(ctx, rpc.BlockNumber(end), false)
	if err != nil {
		log.Debug("Failed to get end block hash while whitelisting", "err", err)
		return hash, fmt.Errorf("%w: %v", errEndBlock, err)
	}

	hash = fmt.Sprintf("%v", block["hash"])

	return hash, nil
}

// Stop the miner if the mining process is running and rewind back the chain
func rewindBack(eth *Ethereum, head uint64, rewindTo uint64) {
	if eth.Miner().Mining() {
		ch := make(chan struct{})
		eth.Miner().Stop(ch)

		<-ch
		rewind(eth, head, rewindTo)

		eth.Miner().Start()
	} else {
		rewind(eth, head, rewindTo)
	}
}

func rewind(eth *Ethereum, head uint64, rewindTo uint64) {
	eth.handler.downloader.Cancel()
	err := eth.blockchain.SetHead(rewindTo)

	if err != nil {
		log.Error("Error while rewinding the chain", "to", rewindTo, "err", err)
	} else {
		rewindLengthMeter.Mark(int64(head - rewindTo))
	}
}

// findCommonAncestorWithFutureMilestones tries to find where the local chain diverged from the milestone chain
// by checking blocks backwards from the milestone range
func findCommonAncestorWithFutureMilestones(eth *Ethereum, start uint64, end uint64, milestoneEndHash string) uint64 {
	// Start from the milestone start block and work backwards
	// to find where our chain matches the expected chain
	if start == 0 {
		return 0
	}

	blockchain := eth.BlockChain()
	db := eth.ChainDb()

	// First, check if we have the correct milestone end block
	localBlock := blockchain.GetBlockByNumber(end)
	if localBlock != nil {
		localHash := localBlock.Hash().Hex()[2:]
		if localHash == milestoneEndHash {
			return end
		}
	}

	// We have a fork. Need to find the actual fork point by checking past milestones
	log.Info("Local chain forked from milestone", "milestone_end", end, "expected_hash", milestoneEndHash)

	// Read future milestone list from database
	// These are milestones that haven't been processed yet but are stored
	futureMilestoneOrder, futureMilestoneList, err := rawdb.ReadFutureMilestoneList(db)
	targetBlock := start - 1
	if err == nil && len(futureMilestoneOrder) > 0 {
		// Check future milestones from newest to oldest
		for i := len(futureMilestoneOrder) - 1; i >= 0; i-- {
			milestoneNum := futureMilestoneOrder[i]
			if milestoneNum >= end {
				continue
			}

			milestoneHash := futureMilestoneList[milestoneNum]
			localBlock := blockchain.GetBlockByNumber(milestoneNum)
			if localBlock != nil && localBlock.Hash() == milestoneHash {
				log.Info("Found matching future milestone", "block", milestoneNum, "hash", milestoneHash)
				return milestoneNum
			}

			if milestoneNum < targetBlock {
				targetBlock = milestoneNum - 1
			}
		}
	}

	if targetBlock < 0 {
		return 0
	}

	return targetBlock
}
